/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements.  See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership.  The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License.  You may obtain a copy of the License at
 *
 *   http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package cloudstack

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/apache/cloudstack-go/v2/cloudstack"
	"k8s.io/cloud-provider/api"
	"k8s.io/klog/v2"
)

const (
	// defaultAsyncJobTimeout is the default time that the CCM waits for a CloudStack async job. It is the same as
	// the default of the cloudstack-go client.
	defaultAsyncJobTimeout = 300 * time.Second

	// pendingJobRetryAfter is the time after which the service controller retries a service that waits for a
	// CloudStack job.
	pendingJobRetryAfter = 60 * time.Second

	// cloudStackParamErrorCode is the error code of CloudStack for an invalid parameter value, for example an
	// unknown job ID.
	cloudStackParamErrorCode = 431

	// maxAsyncJobTimeout is the highest allowed async-job-timeout.
	maxAsyncJobTimeout = 24 * time.Hour
)

// errJobPending means that a CloudStack job for a load balancer rule is still running.
var errJobPending = errors.New("CloudStack job is still running")

// pendingJob is an assign or remove job that was still running when the CCM stopped waiting for it. An empty
// jobID means that a reconcile is changing the rule at the moment (see claim).
type pendingJob struct {
	jobID     string
	op        string
	hostIDs   []string
	submitted time.Time
}

// pendingJobs has the running jobs per load balancer rule ID. When an assign or remove job takes longer than the
// async timeout, CloudStack still runs it. Without this, the next reconcile does not see the change yet and sends
// the same job again, which puts one more load balancer config update in the queue of the virtual router.
// The jobs are only kept in memory. After a restart or a change of the leader, the CCM does not know the running
// jobs, and it can send a job again once. A nil *pendingJobs is valid and disables the tracking.
type pendingJobs struct {
	mu   sync.Mutex
	jobs map[string]pendingJob
}

func newPendingJobs() *pendingJobs {
	return &pendingJobs{jobs: map[string]pendingJob{}}
}

func (p *pendingJobs) get(ruleID string) (pendingJob, bool) {
	if p == nil {
		return pendingJob{}, false
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	job, ok := p.jobs[ruleID]

	return job, ok
}

// claim marks the rule as being changed by this reconcile. It returns false if another reconcile has claimed the
// rule or a job is tracked for it. A nil *pendingJobs always returns true.
func (p *pendingJobs) claim(ruleID string) bool {
	if p == nil {
		return true
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if _, ok := p.jobs[ruleID]; ok {
		return false
	}
	p.jobs[ruleID] = pendingJob{submitted: time.Now()}

	return true
}

// release removes the claim of the rule, unless a job was stored for it in the meantime.
func (p *pendingJobs) release(ruleID string) {
	if p == nil {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if job, ok := p.jobs[ruleID]; ok && job.jobID == "" {
		delete(p.jobs, ruleID)
	}
}

func (p *pendingJobs) set(ruleID string, job pendingJob) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.jobs[ruleID] = job
}

func (p *pendingJobs) delete(ruleID string) {
	if p == nil {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	delete(p.jobs, ruleID)
}

// trackTimedOutJob stores the job if err is the async timeout of cloudstack-go and the job ID is known, and returns
// an error that wraps errJobPending. Else it returns nil.
func (lb *loadBalancer) trackTimedOutJob(lbRule *cloudstack.LoadBalancerRule, op, jobID string, hostIDs []string, err error) error {
	if lb.pendingJobs == nil || jobID == "" || !errors.Is(err, cloudstack.AsyncTimeoutErr) {
		return nil
	}

	lb.pendingJobs.set(lbRule.Id, pendingJob{jobID: jobID, op: op, hostIDs: hostIDs, submitted: time.Now()})
	klog.Infof("The %s job %s for load balancer rule %v is still running after the async timeout, waiting for it", op, jobID, lbRule.Name)

	return fmt.Errorf("%s job %s for load balancer rule %v: %w", op, jobID, lbRule.Name, errJobPending)
}

// checkPendingJob returns an error that wraps errJobPending if the rule has a job that is still running. If the job
// finished or failed, it forgets the job and returns nil, so the rule is reconciled with its current hosts.
func (lb *loadBalancer) checkPendingJob(lbRule *cloudstack.LoadBalancerRule) error {
	job, ok := lb.pendingJobs.get(lbRule.Id)
	if !ok {
		return nil
	}

	if job.jobID == "" {
		return fmt.Errorf("load balancer rule %v is being changed by another reconcile: %w", lbRule.Name, errJobPending)
	}

	r, err := lb.Asyncjob.QueryAsyncJobResult(lb.Asyncjob.NewQueryAsyncJobResultParams(job.jobID))
	if err != nil {
		if isCloudStackError(err, cloudStackParamErrorCode) && strings.Contains(strings.ToLower(err.Error()), "jobid") {
			// CloudStack returns error 431 for an unknown job ID, for example when the job was cleaned up. Other
			// 431 errors keep the job, so it is not sent again.
			klog.Warningf("The %s job %s for load balancer rule %v is not known in CloudStack anymore: %v", job.op, job.jobID, lbRule.Name, err)
			lb.pendingJobs.delete(lbRule.Id)

			return nil
		}

		return fmt.Errorf("error checking %s job %s for load balancer rule %v: %w", job.op, job.jobID, lbRule.Name, err)
	}

	switch r.Jobstatus {
	case 0:
		klog.V(4).Infof("The %s job %s for load balancer rule %v (hosts %v) is still running after %v", job.op, job.jobID, lbRule.Name, job.hostIDs, time.Since(job.submitted).Round(time.Second))

		return fmt.Errorf("%s job %s for load balancer rule %v: %w", job.op, job.jobID, lbRule.Name, errJobPending)
	case 2:
		klog.Warningf("The %s job %s for load balancer rule %v failed: %s", job.op, job.jobID, lbRule.Name, string(r.Jobresult))
	default:
		klog.V(2).Infof("The %s job %s for load balancer rule %v finished after %v", job.op, job.jobID, lbRule.Name, time.Since(job.submitted).Round(time.Second))
	}

	lb.pendingJobs.delete(lbRule.Id)

	return nil
}

// pendingJobsError returns a RetryError for the jobs that are still running, or nil if there are none. The service
// controller retries the service after pendingJobRetryAfter, without an exponential backoff.
func pendingJobsError(pending []string) error {
	if len(pending) == 0 {
		return nil
	}

	return api.NewRetryError("waiting for CloudStack jobs: "+strings.Join(pending, "; "), pendingJobRetryAfter)
}

// keepVMCache returns true if the VM cache stays valid after the error: CloudStack throttled the request, or the
// CCM waits for a running job.
func keepVMCache(err error) bool {
	var re *api.RetryError

	return isAPIThrottled(err) || errors.As(err, &re)
}

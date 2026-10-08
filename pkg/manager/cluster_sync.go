package manager

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/openshift/ci-chat-bot/pkg/utils"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog"
	prowapiv1 "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
	"sigs.k8s.io/prow/pkg/metrics"
)

type recoveredProwJob struct {
	prowJob *prowapiv1.ProwJob
	job     *Job
}

// sync reconstructs manager state from the durable ProwJob records. Conversion
// and scheduling happen before taking m.lock so a slow scheduler cannot block
// admissions or monitor updates.
func (m *jobManager) sync() error {
	if m.prowLister == nil {
		return fmt.Errorf("ProwJob lister is unavailable")
	}
	prowjobs, err := m.prowLister.ProwJobs(m.prowNamespace).List(labels.SelectorFromSet(labels.Set{
		utils.LaunchLabel: "true",
	}))
	if err != nil {
		return err
	}

	now := time.Now()
	entries := make([]recoveredProwJob, 0, len(prowjobs))
	for _, cached := range prowjobs {
		prowJob := cached.DeepCopy()
		job, err := clusterJobFromProwJob(prowJob)
		if err != nil {
			klog.Warningf("Could not reconstruct launch job %s: %v", prowJob.Name, err)
			continue
		}
		if job.BuildCluster == "" {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			job.BuildCluster, err = m.scheduleWithContext(ctx, prowJob.DeepCopy())
			cancel()
			if err != nil {
				klog.Errorf("Unable to recover build cluster for %s: %v", prowJob.Name, err)
				job.BuildCluster = prowJob.Spec.Cluster
			}
		}

		switch prowJob.Status.State {
		case prowapiv1.FailureState, prowapiv1.ErrorState:
			job.Failure = "job failed, see logs"
			job.Complete = true
		case prowapiv1.AbortedState:
			job.Complete = true
			if !job.TerminationRequested {
				job.Failure = "job aborted"
			}
		case prowapiv1.SuccessState:
			job.Complete = true
			job.Failure = ""
		case prowapiv1.SchedulingState, prowapiv1.TriggeredState, prowapiv1.PendingState, "":
			job.State = prowapiv1.PendingState
			job.Failure = ""
		}
		if prowJob.Status.CompletionTime != nil {
			completedAt := prowJob.Status.CompletionTime.Time
			job.CompletedAt = &completedAt
			job.Complete = true
		}
		if job.TerminationRequested {
			job.Complete = true
			job.Failure = "deletion requested"
			job.RequestedChannel = ""
		}
		if job.TerminationRequested || isTerminalProwState(job.State) {
			clearJobAccess(job)
		}
		if !job.ExpiresAt.IsZero() && !now.Before(job.ExpiresAt) {
			continue
		}
		entries = append(entries, recoveredProwJob{prowJob: prowJob, job: job})
	}

	m.lock.Lock()
	if m.jobs == nil {
		m.jobs = make(map[string]*Job)
	}
	if m.requests == nil {
		m.requests = make(map[string]*JobRequest)
	}

	var monitorJobs []Job
	var finishedJobs []Job
	var mceJobs []recoveredProwJob
	for _, entry := range entries {
		job := entry.job
		previous := cloneJob(m.jobs[job.Name])
		if previous != nil && !job.TerminationRequested && !isTerminalProwState(job.State) {
			mergeCachedJobAccess(job, previous)
		}
		if job.TerminationRequested || isTerminalProwState(job.State) {
			clearJobAccess(job)
		}

		if isClusterLaunchMode(job.Mode) && job.RequestedBy != "" {
			existing := m.requests[job.RequestedBy]
			existingJob := (*Job)(nil)
			if existing != nil {
				existingJob = m.jobs[existing.Name]
			}
			if job.TerminationRequested || isTerminalProwState(job.State) || (!job.ExpiresAt.IsZero() && !now.Before(job.ExpiresAt)) {
				if existing != nil && existing.Name == job.Name {
					delete(m.requests, job.RequestedBy)
				}
			} else if existing == nil || existing.Name == job.Name || launchRequestFailure(existingJob) {
				request := jobRequestFromClusterJob(job)
				if request != nil {
					m.requests[job.RequestedBy] = request
				}
			}
		}

		m.jobs[job.Name] = cloneJob(job)
		if job.ManagedClusterName != "" {
			mceJobs = append(mceJobs, entry)
			continue
		}
		if isTerminalProwState(job.State) {
			if !job.TerminationRequested && (previous == nil || previous.State != job.State) && job.State != prowapiv1.AbortedState {
				finishedJobs = append(finishedJobs, *cloneJob(job))
			}
			continue
		}
		if !job.TerminationRequested && (previous == nil || previous.State != job.State || !previous.IsComplete()) {
			monitorJobs = append(monitorJobs, *cloneJob(job))
		}
	}

	for name, job := range m.jobs {
		if job == nil || job.ExpiresAt.IsZero() || now.Before(job.ExpiresAt) {
			continue
		}
		klog.Infof("job %q is expired", name)
		delete(m.jobs, name)
		if request := m.requests[job.RequestedBy]; request != nil && request.Name == name {
			delete(m.requests, job.RequestedBy)
		}
	}
	for user, request := range m.requests {
		if request == nil {
			delete(m.requests, user)
			continue
		}
		if request.RequestedAt.Add(m.maxAge).Before(now) && m.jobs[request.Name] == nil {
			klog.Infof("request %q is expired", user)
			delete(m.requests, user)
		}
	}
	klog.Infof("Job sync complete, %d jobs and %d requests", len(m.jobs), len(m.requests))
	m.lock.Unlock()

	for _, job := range monitorJobs {
		go m.handleJobStartup(job, "sync")
	}
	for _, job := range finishedJobs {
		go m.finishedJob(job)
	}
	for _, entry := range mceJobs {
		m.syncMCEProwJob(entry.prowJob, entry.job)
	}
	return nil
}

// syncMCEProwJob performs the existing MCE completion side effects after the
// ordinary manager lock has been released.
func (m *jobManager) syncMCEProwJob(prowJob *prowapiv1.ProwJob, job *Job) {
	if prowJob == nil || job == nil || job.ManagedClusterName == "" {
		return
	}
	switch prowJob.Status.State {
	case prowapiv1.FailureState, prowapiv1.ErrorState:
		m.mceClusters.lock.RLock()
		cluster := m.mceClusters.clusters[job.ManagedClusterName]
		if cluster != nil {
			cluster = cluster.DeepCopy()
		}
		m.mceClusters.lock.RUnlock()
		if cluster == nil {
			return
		}
		metrics.RecordError(errorMCEImagesetJobRun, m.errorMetric)
		message := fmt.Sprintf("Failed to generate imageset for managed cluster. See logs for details: %s.", prowJob.Status.URL)
		if err := m.deleteManagedCluster(cluster); err != nil {
			message += fmt.Sprintf("\nAn error also occurred when attempting to delete the previously created resources: %v", err)
			klog.Errorf("Failed to delete managed cluster %s: %v", job.ManagedClusterName, err)
		}
		go m.mceSync() //nolint:errcheck
		if m.mceNotifierFn != nil {
			m.mceNotifierFn(cluster, nil, nil, "", "", errors.New(message))
		}
	case prowapiv1.SuccessState:
		m.mceClusters.lock.RLock()
		cluster := m.mceClusters.clusters[job.ManagedClusterName]
		_, deploymentExists := m.mceClusters.deployments[job.ManagedClusterName]
		if cluster != nil {
			cluster = cluster.DeepCopy()
		}
		m.mceClusters.lock.RUnlock()
		if cluster == nil || deploymentExists {
			return
		}

		ciOpNamespace, ok := prowJob.Annotations["ci-chat-bot.openshift.io/ns"]
		if !ok {
			message := fmt.Sprintf("Could not identify ci-operator namespace for job %s.", job.Name)
			klog.Error(message)
			if err := m.deleteManagedCluster(cluster); err != nil {
				message += fmt.Sprintf("\nAn error also occurred when attempting to delete the previously created resources: %v", err)
				klog.Errorf("Failed to delete managed cluster %s: %v", job.ManagedClusterName, err)
			}
			go m.mceSync() //nolint:errcheck
			if m.mceNotifierFn != nil {
				m.mceNotifierFn(cluster, nil, nil, "", "", errors.New(message))
			}
			return
		}

		registryURL := fmt.Sprintf("registry.%s.ci.openshift.org/%s/release:latest", job.BuildCluster, ciOpNamespace)
		if err := m.createCustomImageset(registryURL, job.ManagedClusterName); err != nil {
			metrics.RecordError(errorMCEImagesetCreateRef, m.errorMetric)
			message := fmt.Sprintf("Failed to create imageset for release created by ci-operator: %v.", err)
			klog.Errorf("Failed to create cluster imageset: %v", err)
			if deleteErr := m.deleteManagedCluster(cluster); deleteErr != nil {
				message += fmt.Sprintf("\nAn error also occurred when attempting to delete the previously created resources: %v", deleteErr)
				klog.Errorf("Failed to delete managed cluster %s: %v", job.ManagedClusterName, deleteErr)
			}
			go m.mceSync() //nolint:errcheck
			if m.mceNotifierFn != nil {
				m.mceNotifierFn(cluster, nil, nil, "", "", errors.New(message))
			}
			return
		}
		klog.Infof("Created imageset %s pointing to %s", job.ManagedClusterName, registryURL)

		platform := ""
		switch cluster.Labels["Cloud"] {
		case "Amazon":
			platform = "aws"
		case "Google":
			platform = "gcp"
		}
		if err := m.createClusterDeployment(job.ManagedClusterName, job.ManagedClusterName, cluster.Annotations[utils.BaseDomain], platform); err != nil {
			message := fmt.Sprintf("Failed to create Cluster Deployment: %v", err)
			klog.Errorf("Failed to create cluster deployment: %v", err)
			if deleteErr := m.deleteManagedCluster(cluster); deleteErr != nil {
				message += fmt.Sprintf("\nAn error also occurred when attempting to delete the previously created resources: %v", deleteErr)
				klog.Errorf("Failed to delete managed cluster %s: %v", job.ManagedClusterName, deleteErr)
			}
			go m.mceSync() //nolint:errcheck
			if m.mceNotifierFn != nil {
				m.mceNotifierFn(cluster, nil, nil, "", "", errors.New(message))
			}
			return
		}
		klog.Infof("Created cluster deployment %s", job.ManagedClusterName)
	}
}

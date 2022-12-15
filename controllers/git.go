package controllers

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-logr/logr"
	api "github.com/hybrid-cloud-patterns/patterns-operator/api/v1alpha1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

//go:generate mockgen -source $GOFILE -package=$GOPACKAGE -destination=mock_$GOFILE

var (
	conditionMsgs = map[api.PatternConditionType]string{
		api.GitOutOfSync: "Git repositories are out of sync",
		api.GitInSync:    "Git repositories are in sync"}
)

type repositoryPair struct {
	gitClient                                       GitClient
	name, namespace, origin, target, targetRevision string
	interval                                        time.Duration
	lastCheck, nextCheck                            time.Time
}

func (r repositoryPair) hasDrifted() (bool, error) {
	origin := r.gitClient.NewRemoteClient(&config.RemoteConfig{Name: "origin", URLs: []string{r.origin}})
	target := r.gitClient.NewRemoteClient(&config.RemoteConfig{Name: "target", URLs: []string{r.target}})

	originRefs, err := origin.List(&git.ListOptions{})
	if err != nil {
		return false, err
	}
	if len(originRefs) == 0 {
		return false, fmt.Errorf("no references found for origin %s", r.origin)
	}
	targetRefs, err := target.List(&git.ListOptions{})
	if err != nil {
		return false, err
	}
	if len(targetRefs) == 0 {
		return false, fmt.Errorf("no references found for target %s", r.target)
	}
	originHeadRef := getHeadBranch(originRefs)
	if originHeadRef == nil {
		return false, fmt.Errorf("unable to find %s for origin %s", plumbing.HEAD, r.origin)
	}
	var targetRef *plumbing.Reference
	targetRefName := plumbing.HEAD
	if len(r.targetRevision) > 0 {
		targetRefName = plumbing.NewBranchReferenceName(r.targetRevision)
		targetRef = getReferenceByName(targetRefs, targetRefName)
	} else {
		targetRef = getHeadBranch(targetRefs)
	}
	if targetRef == nil {
		return false, fmt.Errorf("unable to find %s for target %s", targetRefName, r.target)
	}
	return originHeadRef.Hash() != targetRef.Hash(), nil

}

type repositoryPairs []*repositoryPair

func (r repositoryPairs) Len() int {
	return len(r)
}

func (r repositoryPairs) Less(i, j int) bool {
	return r[i].nextCheck.Before(r[j].nextCheck)
}

func (r repositoryPairs) Swap(i, j int) {
	r[i], r[j] = r[j], r[i]
}

type RemoteClient interface {
	List(o *git.ListOptions) (rfs []*plumbing.Reference, err error)
}

type GitClient interface {
	NewRemoteClient(c *config.RemoteConfig) RemoteClient
}

type gitClient struct {
}

func NewGitClient() GitClient {
	return &gitClient{}
}

func (c *gitClient) NewRemoteClient(config *config.RemoteConfig) RemoteClient {
	return git.NewRemote(nil, config)
}

type driftWatcher struct {
	kcli client.Client
	//endCh is used to notify the watch routine to exit the loop
	endCh, updateCh chan interface{}
	repoPairs       repositoryPairs
	mutex           *sync.Mutex
	logger          logr.Logger
	timer           *time.Timer
	gitClient       GitClient
}

func NewDriftWatcher(kubeClient client.Client, logger logr.Logger, gitClient GitClient) DriftWatcher {
	return &driftWatcher{
		kcli:      kubeClient,
		logger:    logger,
		repoPairs: repositoryPairs{},
		endCh:     make(chan interface{}),
		mutex:     &sync.Mutex{},
		gitClient: gitClient}
}

type DriftWatcher interface {
	add(name, namespace, origin, target, targetRevision string, interval int) error
	remove(name, namespace string) error
	watch() chan interface{}
	isWatching(name, namespace string) bool
}

// isWatching returns true if the pair name,namespace reference is being monitored for drifts, false otherwise
func (d *driftWatcher) isWatching(name, namespace string) bool {
	d.mutex.Lock()
	defer d.mutex.Unlock()
	for _, item := range d.repoPairs {
		if item.name == name && item.namespace == namespace {
			return true
		}
	}
	return false
}

// add instructs the client to start monitoring for drifts between two repositories
func (d *driftWatcher) add(name, namespace, origin, target, targetRevision string, interval int) error {
	if d.updateCh == nil {
		return fmt.Errorf("unable to add %s in %s when watch has not yet started", name, namespace)
	}
	d.mutex.Lock()
	defer d.mutex.Unlock()
	pair := repositoryPair{
		name:           name,
		namespace:      namespace,
		origin:         origin,
		target:         target,
		targetRevision: targetRevision,
		interval:       time.Duration(interval) * time.Second,
		nextCheck:      time.Now().Add(time.Duration(interval) * time.Second),
		gitClient:      d.gitClient}
	d.repoPairs = append(d.repoPairs, &pair)
	sort.Sort(d.repoPairs)
	// Notify of updates
	d.updateCh <- struct{}{}
	return nil
}

// remove instructs the client to stop monitoring for drifts for the given resource name and namespace
func (d *driftWatcher) remove(name, namespace string) error {
	if d.updateCh == nil {
		return fmt.Errorf("unable to remove %s in %s when watch has not yet started", name, namespace)
	}
	d.mutex.Lock()
	defer d.mutex.Unlock()
	for index := range d.repoPairs {
		if name == d.repoPairs[index].name && namespace == d.repoPairs[index].namespace {
			d.repoPairs = append(d.repoPairs[:index], d.repoPairs[index+1:]...)
			sort.Sort(d.repoPairs)
			// Notify of updates
			d.updateCh <- struct{}{}
			return nil
		}
	}
	return fmt.Errorf("unable to find git remote pair for pattern %s in namespace %s", name, namespace)
}

func (d *driftWatcher) startNewTimer() {
	d.mutex.Lock()
	defer d.mutex.Unlock()
	// if there is an ongoing timer...
	if d.timer != nil {
		// ...stop the timer. Any ongoing timer is no longer valid as there have been changes to the pair slice
		d.timer.Stop()
	}
	if len(d.repoPairs) == 0 {
		return
	}
	// slice is already sorted from a previous call to Add or Remove or from a previous timer
	nextPair := d.repoPairs[0]
	nextInterval := time.Until(nextPair.nextCheck)
	if time.Now().After(nextPair.nextCheck) {
		// In case there is an overdue check, which would result in a negative value, we set it to 0 so that it is triggered right away
		d.logger.V(1).Info(fmt.Sprintf("Next interval is negative, resetting to 0 %s: %s - %s\n", nextInterval.String(), time.Now().String(), nextPair.nextCheck.String()))
		nextInterval = 0
	}
	// start a timer and execute drift check when timer expires
	d.timer = time.AfterFunc(nextInterval, func() {
		d.mutex.Lock()
		defer d.mutex.Unlock()
		if len(d.repoPairs) == 0 {
			d.updateCh <- struct{}{}
			return
		}
		pair := d.repoPairs[0]
		hasDrifted, err := pair.hasDrifted()
		if err != nil {
			d.logger.Error(err, "found error while detecting drift")
		} else {
			conditionType := api.GitInSync
			if hasDrifted {
				d.logger.Info(fmt.Sprintf("git repositories have drifted for resource %s in namespace %s", pair.name, pair.namespace))
				conditionType = api.GitOutOfSync
			}
			err := updatePatternConditions(d.kcli, conditionType, pair.name, pair.namespace, time.Now())
			if err != nil {
				d.logger.Error(err, fmt.Sprintf("failed to update pattern condition for %s in namespace %s", pair.name, pair.namespace))
			}
		}
		pair.lastCheck = time.Now()
		pair.nextCheck = pair.lastCheck.Add(pair.interval)
		d.repoPairs[0] = pair
		// recalculate next timer
		sort.Sort(d.repoPairs)
		d.updateCh <- struct{}{}
	})
	d.logger.V(1).Info(fmt.Sprintf("New timer started for %s in %s to end on %s", nextPair.name, nextPair.namespace, nextPair.nextCheck.String()))
}

// watch starts the process of monitoring the drifts. The call returns a channel to be used to manage
// the closure of the monitoring routine cleanly.
func (d *driftWatcher) watch() chan interface{} {
	// ready to start processing notifications
	d.updateCh = make(chan interface{})
	go func() {
		for {
			select {
			case <-d.endCh:
				if d.timer != nil {
					d.timer.Stop()
				}
				return
			case <-d.updateCh:
				go d.startNewTimer()
			}
		}
	}()
	d.updateCh <- struct{}{}
	return d.endCh
}

func updatePatternConditions(kcli client.Client, conditionType api.PatternConditionType, name, namespace string, timestamp time.Time) error {
	var pattern api.Pattern
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// fetch the pattern object
	err := kcli.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &pattern)
	if err != nil {
		return err
	}
	// get the current active condition
	currentIndex, currentCondition := getPatternConditionByStatus(pattern.Status.Conditions, v1.ConditionTrue)
	if currentCondition != nil && currentCondition.Type != conditionType {
		// mark the current condition with status false and update timestamp
		currentCondition.Status = v1.ConditionFalse
		currentCondition.LastUpdateTime = metav1.Time{Time: timestamp}
		pattern.Status.Conditions[currentIndex] = *currentCondition
	}
	// get the condition by status
	index, condition := getPatternConditionByType(pattern.Status.Conditions, conditionType)
	if condition == nil {
		// condition not yet found, so we create a new one
		condition = &api.PatternCondition{
			Type:               conditionType,
			Status:             v1.ConditionTrue,
			LastUpdateTime:     metav1.Time{Time: timestamp},
			LastTransitionTime: metav1.Time{Time: timestamp},
			Message:            conditionMsgs[conditionType]}
		pattern.Status.Conditions = append(pattern.Status.Conditions, *condition)
		return kcli.Status().Update(ctx, &pattern)
	}
	condition.LastUpdateTime = metav1.Time{Time: timestamp}
	if condition.Status == v1.ConditionTrue {
		pattern.Status.Conditions[index] = *condition
		return kcli.Status().Update(ctx, &pattern)
	}
	// Not current condition, so we make it so
	condition.Status = v1.ConditionTrue
	condition.LastTransitionTime = metav1.Time{Time: timestamp}
	pattern.Status.Conditions[index] = *condition
	return kcli.Status().Update(ctx, &pattern)
}

func getHeadBranch(refs []*plumbing.Reference) *plumbing.Reference {
	headRef := getReferenceByName(refs, plumbing.HEAD)
	if headRef == nil {
		return nil
	}
	return getReferenceByName(refs, headRef.Target())
}
func getReferenceByName(refs []*plumbing.Reference, referenceName plumbing.ReferenceName) *plumbing.Reference {
	for _, ref := range refs {
		if ref.Name() == referenceName {
			return ref
		}
	}
	return nil
}

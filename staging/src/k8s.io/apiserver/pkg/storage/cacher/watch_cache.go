/*
Copyright 2015 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cacher

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/btree"
	"k8s.io/apimachinery/pkg/api/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/storage"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
	utiltrace "k8s.io/utils/trace"
)

const (
	// blockTimeout determines how long we're willing to block the request
	// to wait for a given resource version to be propagated to cache,
	// before terminating request and returning Timeout error with retry
	// after suggestion.
	blockTimeout = 3 * time.Second

	// resourceVersionTooHighRetrySeconds is the seconds before a operation should be retried by the client
	// after receiving a 'too high resource version' error.
	resourceVersionTooHighRetrySeconds = 1

	// eventFreshDuration is time duration of events we want to keep.
	// We set it to `defaultBookmarkFrequency` plus epsilon to maximize
	// chances that last bookmark was sent within kept history, at the
	// same time, minimizing the needed memory usage.
	eventFreshDuration = 75 * time.Second

	// defaultLowerBoundCapacity is a default value for event cache capacity's lower bound.
	// TODO: Figure out, to what value we can decreased it.
	defaultLowerBoundCapacity = 100

	// defaultUpperBoundCapacity  should be able to keep eventFreshDuration of history.
	defaultUpperBoundCapacity = 100 * 1024
)

// watchCacheEvent is a single "watch event" that is send to users of
// watchCache. Additionally to a typical "watch.Event" it contains
// the previous value of the object to enable proper filtering in the
// upper layers.
type watchCacheEvent struct {
	Type            watch.EventType
	Object          runtime.Object
	ObjLabels       labels.Set
	ObjFields       fields.Set
	PrevObject      runtime.Object
	PrevObjLabels   labels.Set
	PrevObjFields   fields.Set
	Key             string
	ResourceVersion uint64
	RecordTime      time.Time
}

// Computing a key of an object is generally non-trivial (it performs
// e.g. validation underneath). Similarly computing object fields and
// labels. To avoid computing them multiple times (to serve the event
// in different List/Watch requests), in the underlying store we are
// keeping structs (key, object, labels, fields).
type storeElement struct {
	Key    string
	Object runtime.Object
	Labels labels.Set
	Fields fields.Set
}

func (t *storeElement) Less(than btree.Item) bool {
	return t.Key < than.(*storeElement).Key
}

var _ btree.Item = (*storeElement)(nil)

func storeElementKey(obj interface{}) (string, error) {
	elem, ok := obj.(*storeElement)
	if !ok {
		return "", fmt.Errorf("not a storeElement: %v", obj)
	}
	return elem.Key, nil
}

func storeElementObject(obj interface{}) (runtime.Object, error) {
	elem, ok := obj.(*storeElement)
	if !ok {
		return nil, fmt.Errorf("not a storeElement: %v", obj)
	}
	return elem.Object, nil
}

func storeElementIndexFunc(objIndexFunc cache.IndexFunc) cache.IndexFunc {
	return func(obj interface{}) (strings []string, e error) {
		seo, err := storeElementObject(obj)
		if err != nil {
			return nil, err
		}
		return objIndexFunc(seo)
	}
}

func storeElementIndexers(indexers *cache.Indexers) cache.Indexers {
	if indexers == nil {
		return cache.Indexers{}
	}
	ret := cache.Indexers{}
	for indexName, indexFunc := range *indexers {
		ret[indexName] = storeElementIndexFunc(indexFunc)
	}
	return ret
}

// watchCache implements a Store interface.
// However, it depends on the elements implementing runtime.Object interface.
//
// watchCache is a "sliding window" (with a limited capacity) of objects
// observed from a watch.
type watchCache struct {
	sync.RWMutex

	// Condition on which lists are waiting for the fresh enough
	// resource version.
	cond *sync.Cond

	// Maximum size of history window.
	capacity int

	// upper bound of capacity since event cache has a dynamic size.
	upperBoundCapacity int

	// lower bound of capacity since event cache has a dynamic size.
	lowerBoundCapacity int

	// keyFunc is used to get a key in the underlying storage for a given object.
	keyFunc func(runtime.Object) (string, error)

	// getAttrsFunc is used to get labels and fields of an object.
	getAttrsFunc func(runtime.Object) (labels.Set, fields.Set, error)

	// cache is used a cyclic buffer - its first element (with the smallest
	// resourceVersion) is defined by startIndex, its last element is defined
	// by endIndex (if cache is full it will be startIndex + capacity).
	// Both startIndex and endIndex can be greater than buffer capacity -
	// you should always apply modulo capacity to get an index in cache array.
	cache      []*watchCacheEvent
	startIndex int
	endIndex   int

	// store will effectively support LIST operation from the "end of cache
	// history" i.e. from the moment just after the newest cached watched event.
	// It is necessary to effectively allow clients to start watching at now.
	// NOTE: We assume that <store> is thread-safe.
	store btreeIndexer

	// ResourceVersion up to which the watchCache is propagated.
	resourceVersion uint64

	// ResourceVersion of the last list result (populated via Replace() method).
	listResourceVersion uint64

	// This handler is run at the end of every successful Replace() method.
	onReplace func()

	// This handler is run at the end of every Add/Update/Delete method
	// and additionally gets the previous value of the object.
	eventHandler func(*watchCacheEvent)

	// for testing timeouts.
	clock clock.Clock

	// An underlying storage.Versioner.
	versioner storage.Versioner

	// cacher's objectType.
	objectType reflect.Type

	// For testing cache interval invalidation.
	indexValidator indexValidator

	continueCache *continueCache
}

func newWatchCache(
	keyFunc func(runtime.Object) (string, error),
	eventHandler func(*watchCacheEvent),
	getAttrsFunc func(runtime.Object) (labels.Set, fields.Set, error),
	versioner storage.Versioner,
	indexers *cache.Indexers,
	clock clock.Clock,
	objectType reflect.Type) *watchCache {
	wc := &watchCache{
		capacity:           defaultLowerBoundCapacity,
		keyFunc:            keyFunc,
		getAttrsFunc:       getAttrsFunc,
		cache:              make([]*watchCacheEvent, defaultLowerBoundCapacity),
		lowerBoundCapacity: defaultLowerBoundCapacity,
		upperBoundCapacity: defaultUpperBoundCapacity,
		startIndex:         0,
		endIndex:           0,
		// store:               cache.NewIndexer(storeElementKey, storeElementIndexers(indexers)),
		store:               newBtreeStore(2), // TODO: figure out what the degree should be.
		resourceVersion:     0,
		listResourceVersion: 0,
		eventHandler:        eventHandler,
		clock:               clock,
		versioner:           versioner,
		objectType:          objectType,
		continueCache:       newContinueCache(),
	}
	objType := objectType.String()
	watchCacheCapacity.WithLabelValues(objType).Set(float64(wc.capacity))
	wc.cond = sync.NewCond(wc.RLocker())
	wc.indexValidator = wc.isIndexValidLocked

	return wc
}

// Add takes runtime.Object as an argument.
func (w *watchCache) Add(obj interface{}) error {
	object, resourceVersion, err := w.objectToVersionedRuntimeObject(obj)
	if err != nil {
		return err
	}
	event := watch.Event{Type: watch.Added, Object: object}

	f := func(elem *storeElement) error { return w.store.Add(elem) }
	return w.processEvent(event, resourceVersion, f)
}

// Update takes runtime.Object as an argument.
func (w *watchCache) Update(obj interface{}) error {
	object, resourceVersion, err := w.objectToVersionedRuntimeObject(obj)
	if err != nil {
		return err
	}
	event := watch.Event{Type: watch.Modified, Object: object}

	f := func(elem *storeElement) error { return w.store.Update(elem) }
	return w.processEvent(event, resourceVersion, f)
}

// Delete takes runtime.Object as an argument.
func (w *watchCache) Delete(obj interface{}) error {
	object, resourceVersion, err := w.objectToVersionedRuntimeObject(obj)
	if err != nil {
		return err
	}
	event := watch.Event{Type: watch.Deleted, Object: object}

	f := func(elem *storeElement) error { return w.store.Delete(elem) }
	return w.processEvent(event, resourceVersion, f)
}

func (w *watchCache) objectToVersionedRuntimeObject(obj interface{}) (runtime.Object, uint64, error) {
	object, ok := obj.(runtime.Object)
	if !ok {
		return nil, 0, fmt.Errorf("obj does not implement runtime.Object interface: %v", obj)
	}
	resourceVersion, err := w.versioner.ObjectResourceVersion(object)
	if err != nil {
		return nil, 0, err
	}
	return object, resourceVersion, nil
}

// processEvent is safe as long as there is at most one call to it in flight
// at any point in time.
func (w *watchCache) processEvent(event watch.Event, resourceVersion uint64, updateFunc func(*storeElement) error) error {
	key, err := w.keyFunc(event.Object)
	if err != nil {
		return fmt.Errorf("couldn't compute key: %v", err)
	}
	elem := &storeElement{Key: key, Object: event.Object}
	elem.Labels, elem.Fields, err = w.getAttrsFunc(event.Object)
	if err != nil {
		return err
	}

	wcEvent := &watchCacheEvent{
		Type:            event.Type,
		Object:          elem.Object,
		ObjLabels:       elem.Labels,
		ObjFields:       elem.Fields,
		Key:             key,
		ResourceVersion: resourceVersion,
		RecordTime:      w.clock.Now(),
	}

	if err := func() error {
		// TODO: We should consider moving this lock below after the watchCacheEvent
		// is created. In such situation, the only problematic scenario is Replace(
		// happening after getting object from store and before acquiring a lock.
		// Maybe introduce another lock for this purpose.
		w.Lock()
		defer w.Unlock()

		previous, exists, err := w.store.Get(elem)
		if err != nil {
			return err
		}
		if exists {
			previousElem := previous.(*storeElement)
			wcEvent.PrevObject = previousElem.Object
			wcEvent.PrevObjLabels = previousElem.Labels
			wcEvent.PrevObjFields = previousElem.Fields
		}

		w.updateCache(wcEvent)
		w.resourceVersion = resourceVersion
		defer w.cond.Broadcast()

		return updateFunc(elem)
	}(); err != nil {
		return err
	}

	// Avoid calling event handler under lock.
	// This is safe as long as there is at most one call to Add/Update/Delete and
	// UpdateResourceVersion in flight at any point in time, which is true now,
	// because reflector calls them synchronously from its main thread.
	if w.eventHandler != nil {
		w.eventHandler(wcEvent)
	}
	return nil
}

// Assumes that lock is already held for write.
func (w *watchCache) updateCache(event *watchCacheEvent) {
	w.resizeCacheLocked(event.RecordTime)
	if w.isCacheFullLocked() {
		oldestRV := w.cache[w.startIndex%w.capacity].ResourceVersion
		w.continueCache.cleanup(oldestRV)
		// Cache is full - remove the oldest element.
		w.startIndex++
	}
	w.cache[w.endIndex%w.capacity] = event
	w.endIndex++
}

// resizeCacheLocked resizes the cache if necessary:
// - increases capacity by 2x if cache is full and all cached events occurred within last eventFreshDuration.
// - decreases capacity by 2x when recent quarter of events occurred outside of eventFreshDuration(protect watchCache from flapping).
func (w *watchCache) resizeCacheLocked(eventTime time.Time) {
	if w.isCacheFullLocked() && eventTime.Sub(w.cache[w.startIndex%w.capacity].RecordTime) < eventFreshDuration {
		capacity := min(w.capacity*2, w.upperBoundCapacity)
		if capacity > w.capacity {
			w.doCacheResizeLocked(capacity)
		}
		return
	}
	if w.isCacheFullLocked() && eventTime.Sub(w.cache[(w.endIndex-w.capacity/4)%w.capacity].RecordTime) > eventFreshDuration {
		capacity := max(w.capacity/2, w.lowerBoundCapacity)
		if capacity < w.capacity {
			w.doCacheResizeLocked(capacity)
		}
		return
	}
}

// isCacheFullLocked used to judge whether watchCacheEvent is full.
// Assumes that lock is already held for write.
func (w *watchCache) isCacheFullLocked() bool {
	return w.endIndex == w.startIndex+w.capacity
}

// doCacheResizeLocked resize watchCache's event array with different capacity.
// Assumes that lock is already held for write.
func (w *watchCache) doCacheResizeLocked(capacity int) {
	newCache := make([]*watchCacheEvent, capacity)
	if capacity < w.capacity {
		// adjust startIndex if cache capacity shrink.
		w.startIndex = w.endIndex - capacity
	}
	for i := w.startIndex; i < w.endIndex; i++ {
		newCache[i%capacity] = w.cache[i%w.capacity]
	}
	w.cache = newCache
	recordsWatchCacheCapacityChange(w.objectType.String(), w.capacity, capacity)
	w.capacity = capacity
}

func (w *watchCache) UpdateResourceVersion(resourceVersion string) {
	rv, err := w.versioner.ParseResourceVersion(resourceVersion)
	if err != nil {
		klog.Errorf("Couldn't parse resourceVersion: %v", err)
		return
	}

	func() {
		w.Lock()
		defer w.Unlock()
		w.resourceVersion = rv
	}()

	// Avoid calling event handler under lock.
	// This is safe as long as there is at most one call to Add/Update/Delete and
	// UpdateResourceVersion in flight at any point in time, which is true now,
	// because reflector calls them synchronously from its main thread.
	if w.eventHandler != nil {
		wcEvent := &watchCacheEvent{
			Type:            watch.Bookmark,
			ResourceVersion: rv,
		}
		w.eventHandler(wcEvent)
	}
}

// List returns list of pointers to <storeElement> objects.
func (w *watchCache) List() []interface{} {
	return w.store.List()
}

// waitUntilFreshAndBlock waits until cache is at least as fresh as given <resourceVersion>.
// NOTE: This function acquired lock and doesn't release it.
// You HAVE TO explicitly call w.RUnlock() after this function.
func (w *watchCache) waitUntilFreshAndBlock(resourceVersion uint64, trace *utiltrace.Trace) error {
	startTime := w.clock.Now()

	// In case resourceVersion is 0, we accept arbitrarily stale result.
	// As a result, the condition in the below for loop will never be
	// satisfied (w.resourceVersion is never negative), this call will
	// never hit the w.cond.Wait().
	// As a result - we can optimize the code by not firing the wakeup
	// function (and avoid starting a gorotuine), especially given that
	// resourceVersion=0 is the most common case.
	if resourceVersion > 0 {
		go func() {
			// Wake us up when the time limit has expired.  The docs
			// promise that time.After (well, NewTimer, which it calls)
			// will wait *at least* the duration given. Since this go
			// routine starts sometime after we record the start time, and
			// it will wake up the loop below sometime after the broadcast,
			// we don't need to worry about waking it up before the time
			// has expired accidentally.
			<-w.clock.After(blockTimeout)
			w.cond.Broadcast()
		}()
	}

	w.RLock()
	if trace != nil {
		trace.Step("watchCache locked acquired")
	}
	for w.resourceVersion < resourceVersion {
		if w.clock.Since(startTime) >= blockTimeout {
			// Request that the client retry after 'resourceVersionTooHighRetrySeconds' seconds.
			return storage.NewTooLargeResourceVersionError(resourceVersion, w.resourceVersion, resourceVersionTooHighRetrySeconds)
		}
		w.cond.Wait()
	}
	if trace != nil {
		trace.Step("watchCache fresh enough")
	}
	return nil
}

// WaitUntilFreshAndList returns list of pointers to `storeElement` objects along
// with their ResourceVersion and the name of the index, if any, that was used.
func (w *watchCache) WaitUntilFreshAndList(resourceVersion uint64, key string, listOpts storage.ListOptions, trace *utiltrace.Trace) ([]interface{}, uint64, string, error) {
	err := w.waitUntilFreshAndBlock(resourceVersion, trace)
	if err != nil {
		return nil, 0, "", err
	}

	pred := listOpts.Predicate
	hasContinuation := len(pred.Continue) > 0
	hasLimit := pred.Limit > 0

	if !hasLimit {
		return w.store.List(), w.resourceVersion, "", nil
	}

	// Perform a clone under the lock, this should be a relatively
	// inexpensive operation since the implementation of clone uses
	// copy on write semantics. Once cloned, serve the list from the
	// cloned copy to avoid building the response under a lock.
	var storeClone btreeIndexer
	if err := func() error {
		defer w.RUnlock()
		if hasContinuation {
			if _, ok := w.continueCache.cache[resourceVersion]; !ok {
				// We return a 410 Gone here for the following reason:
				//
				// Before the LIST request reaches the watchCache, we
				// check if it should be delegated to etcd directly. In
				// this check, we see if the request has a continuation,
				// if it does, we check if the RV of the continuation
				// token is still present in the watchCache or not, if
				// it isn't then we let etcd serve the request.
				//
				// As and when events are removed from the watchCache
				// (when it becomes full), we also check and evict the
				// cached copy of the tree for the resource version whose
				// event is going to be removed.
				//
				// Due to this, in case the cached clone is evicted, we
				// return a 410 Gone similar to when a continue token
				// expires. On receiving this error, the client can retry
				// and on this retry, the check for delegation will route
				// the request to etcd and things proceed accordingly.
				return errors.NewResourceExpired(fmt.Sprintf("too old resource version: %d", resourceVersion))
			}
			storeClone = w.continueCache.cache[resourceVersion]
			return nil
		}
		storeClone = w.store.Clone()
		w.continueCache.cache[resourceVersion] = storeClone

		return nil
	}(); err != nil {
		return nil, 0, "", err
	}

	if !strings.HasSuffix(key, "/") {
		key += "/"
	}

	if hasContinuation {
		continueKey, _, err := decodeContinue(pred.Continue, key)
		if err != nil {
			return nil, 0, "", apierrors.NewBadRequest(fmt.Sprintf("invalid continue token: %v", err))
		}
		key = continueKey
	}

	return storeClone.LimitPrefixRead(listOpts.Predicate.Limit, key), w.resourceVersion, "", nil
}

// WaitUntilFreshAndGet returns a pointers to <storeElement> object.
func (w *watchCache) WaitUntilFreshAndGet(resourceVersion uint64, key string, trace *utiltrace.Trace) (interface{}, bool, uint64, error) {
	err := w.waitUntilFreshAndBlock(resourceVersion, trace)
	defer w.RUnlock()
	if err != nil {
		return nil, false, 0, err
	}
	value, exists, err := w.store.GetByKey(key)
	return value, exists, w.resourceVersion, err
}

func (w *watchCache) ListKeys() []string {
	return w.store.ListKeys()
}

// Get takes runtime.Object as a parameter. However, it returns
// pointer to <storeElement>.
func (w *watchCache) Get(obj interface{}) (interface{}, bool, error) {
	object, ok := obj.(runtime.Object)
	if !ok {
		return nil, false, fmt.Errorf("obj does not implement runtime.Object interface: %v", obj)
	}
	key, err := w.keyFunc(object)
	if err != nil {
		return nil, false, fmt.Errorf("couldn't compute key: %v", err)
	}

	return w.store.Get(&storeElement{Key: key, Object: object})
}

// GetByKey returns pointer to <storeElement>.
func (w *watchCache) GetByKey(key string) (interface{}, bool, error) {
	return w.store.GetByKey(key)
}

// Replace takes slice of runtime.Object as a parameter.
func (w *watchCache) Replace(objs []interface{}, resourceVersion string) error {
	version, err := w.versioner.ParseResourceVersion(resourceVersion)
	if err != nil {
		return err
	}

	toReplace := make([]interface{}, 0, len(objs))
	for _, obj := range objs {
		object, ok := obj.(runtime.Object)
		if !ok {
			return fmt.Errorf("didn't get runtime.Object for replace: %#v", obj)
		}
		key, err := w.keyFunc(object)
		if err != nil {
			return fmt.Errorf("couldn't compute key: %v", err)
		}
		objLabels, objFields, err := w.getAttrsFunc(object)
		if err != nil {
			return err
		}
		toReplace = append(toReplace, &storeElement{
			Key:    key,
			Object: object,
			Labels: objLabels,
			Fields: objFields,
		})
	}

	w.Lock()
	defer w.Unlock()

	w.startIndex = 0
	w.endIndex = 0
	if err := w.store.Replace(toReplace, resourceVersion); err != nil {
		return err
	}
	w.listResourceVersion = version
	w.resourceVersion = version
	if w.onReplace != nil {
		w.onReplace()
	}
	w.cond.Broadcast()
	klog.V(3).Infof("Replace watchCache (rev: %v) ", resourceVersion)
	return nil
}

func (w *watchCache) SetOnReplace(onReplace func()) {
	w.Lock()
	defer w.Unlock()
	w.onReplace = onReplace
}

func (w *watchCache) Resync() error {
	// Nothing to do
	return nil
}

// isIndexValidLocked checks if a given index is still valid.
// This assumes that the lock is held.
func (w *watchCache) isIndexValidLocked(index int) bool {
	return index >= w.startIndex
}

// getAllEventsSinceLocked returns a watchCacheInterval that can be used to
// retrieve events since a certain resourceVersion. This function assumes to
// be called under the watchCache lock.
func (w *watchCache) getAllEventsSinceLocked(resourceVersion uint64) (*watchCacheInterval, error) {
	size := w.endIndex - w.startIndex
	var oldest uint64
	switch {
	case w.listResourceVersion > 0 && w.startIndex == 0:
		// If no event was removed from the buffer since last relist, the oldest watch
		// event we can deliver is one greater than the resource version of the list.
		oldest = w.listResourceVersion + 1
	case size > 0:
		// If the previous condition is not satisfied: either some event was already
		// removed from the buffer or we've never completed a list (the latter can
		// only happen in unit tests that populate the buffer without performing
		// list/replace operations), the oldest watch event we can deliver is the first
		// one in the buffer.
		oldest = w.cache[w.startIndex%w.capacity].ResourceVersion
	default:
		return nil, fmt.Errorf("watch cache isn't correctly initialized")
	}

	if resourceVersion == 0 {
		// resourceVersion = 0 means that we don't require any specific starting point
		// and we would like to start watching from ~now.
		// However, to keep backward compatibility, we additionally need to return the
		// current state and only then start watching from that point.
		//
		// TODO: In v2 api, we should stop returning the current state - #13969.
		ci, err := newCacheIntervalFromStore(w.resourceVersion, w.store, w.getAttrsFunc)
		if err != nil {
			return nil, err
		}
		return ci, nil
	}
	if resourceVersion < oldest-1 {
		return nil, errors.NewResourceExpired(fmt.Sprintf("too old resource version: %d (%d)", resourceVersion, oldest-1))
	}

	// Binary search the smallest index at which resourceVersion is greater than the given one.
	f := func(i int) bool {
		return w.cache[(w.startIndex+i)%w.capacity].ResourceVersion > resourceVersion
	}
	first := sort.Search(size, f)
	indexerFunc := func(i int) *watchCacheEvent {
		return w.cache[i%w.capacity]
	}
	ci := newCacheInterval(w.startIndex+first, w.endIndex, indexerFunc, w.indexValidator, &w.RWMutex)
	return ci, nil
}

// isRVValid checks if rv is still in the watchCache.
func (w *watchCache) isRVValid(rv uint64) bool {
	w.RLock()
	defer w.Unlock()

	// Binary search if the rv is present in the watchCache.
	f := func(i int) bool {
		return w.cache[(w.startIndex+i)%w.capacity].ResourceVersion == rv
	}
	size := w.endIndex - w.startIndex
	return sort.Search(size, f) != size
}

type continueToken struct {
	APIVersion      string `json:"v"`
	ResourceVersion int64  `json:"rv"`
	StartKey        string `json:"start"`
}

// parseFrom transforms an encoded predicate from into a versioned struct.
// TODO: return a typed error that instructs clients that they must relist
func decodeContinue(continueValue, keyPrefix string) (fromKey string, rv int64, err error) {
	data, err := base64.RawURLEncoding.DecodeString(continueValue)
	if err != nil {
		return "", 0, fmt.Errorf("continue key is not valid: %v", err)
	}
	var c continueToken
	if err := json.Unmarshal(data, &c); err != nil {
		return "", 0, fmt.Errorf("continue key is not valid: %v", err)
	}
	switch c.APIVersion {
	case "meta.k8s.io/v1":
		if c.ResourceVersion == 0 {
			return "", 0, fmt.Errorf("continue key is not valid: incorrect encoded start resourceVersion (version meta.k8s.io/v1)")
		}
		if len(c.StartKey) == 0 {
			return "", 0, fmt.Errorf("continue key is not valid: encoded start key empty (version meta.k8s.io/v1)")
		}
		// defend against path traversal attacks by clients - path.Clean will ensure that startKey cannot
		// be at a higher level of the hierarchy, and so when we append the key prefix we will end up with
		// continue start key that is fully qualified and cannot range over anything less specific than
		// keyPrefix.
		key := c.StartKey
		if !strings.HasPrefix(key, "/") {
			key = "/" + key
		}
		cleaned := path.Clean(key)
		if cleaned != key {
			return "", 0, fmt.Errorf("continue key is not valid: %s", c.StartKey)
		}
		return keyPrefix + cleaned[1:], c.ResourceVersion, nil
	default:
		return "", 0, fmt.Errorf("continue key is not valid: server does not recognize this encoded version %q", c.APIVersion)
	}
}

func getRVFromContinue(continueValue string) (uint64, error) {
	data, err := base64.RawURLEncoding.DecodeString(continueValue)
	if err != nil {
		return 0, fmt.Errorf("continue key is not valid: %v", err)
	}

	var c continueToken
	if err := json.Unmarshal(data, &c); err != nil {
		return 0, fmt.Errorf("continue key is not valid: %v", err)
	}

	if c.APIVersion != "meta.k8s.io/v1" {
		return 0, fmt.Errorf("continue key is not valid: server does not recognize this encoded version %q", c.APIVersion)
	}

	return uint64(c.ResourceVersion), nil
}

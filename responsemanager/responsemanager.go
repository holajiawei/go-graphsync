package responsemanager

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/ipfs/go-graphsync/responsemanager/blockhooks"

	"github.com/ipfs/go-graphsync"
	"github.com/ipfs/go-graphsync/ipldutil"
	gsmsg "github.com/ipfs/go-graphsync/message"
	"github.com/ipfs/go-graphsync/responsemanager/peerresponsemanager"
	"github.com/ipfs/go-graphsync/responsemanager/requesthooks.go"
	"github.com/ipfs/go-graphsync/responsemanager/runtraversal"
	logging "github.com/ipfs/go-log"
	"github.com/ipfs/go-peertaskqueue/peertask"
	ipld "github.com/ipld/go-ipld-prime"
	cidlink "github.com/ipld/go-ipld-prime/linking/cid"
	"github.com/libp2p/go-libp2p-core/peer"
)

var log = logging.Logger("graphsync")

const (
	maxInProcessRequests = 6
	thawSpeed            = time.Millisecond * 100
)

type inProgressResponseStatus struct {
	ctx       context.Context
	cancelFn  func()
	request   gsmsg.GraphSyncRequest
	loader    ipld.Loader
	traverser ipldutil.Traverser
	isPaused  bool
}

type responseKey struct {
	p         peer.ID
	requestID graphsync.RequestID
}

type responseTaskData struct {
	ctx       context.Context
	request   gsmsg.GraphSyncRequest
	loader    ipld.Loader
	traverser ipldutil.Traverser
}

// QueryQueue is an interface that can receive new selector query tasks
// and prioritize them as needed, and pop them off later
type QueryQueue interface {
	PushTasks(to peer.ID, tasks ...peertask.Task)
	PopTasks(targetMinWork int) (peer.ID, []*peertask.Task, int)
	Remove(topic peertask.Topic, p peer.ID)
	TasksDone(to peer.ID, tasks ...*peertask.Task)
	ThawRound()
}

// RequestHooks is an interface for processing request hooks
type RequestHooks interface {
	ProcessRequestHooks(p peer.ID, request graphsync.RequestData) requesthooks.Result
}

// BlockHooks is an interface for processing block hooks
type BlockHooks interface {
	ProcessBlockHooks(p peer.ID, request graphsync.RequestData, blockData graphsync.BlockData) blockhooks.Result
}

// PeerManager is an interface that returns sender interfaces for peer responses.
type PeerManager interface {
	SenderForPeer(p peer.ID) peerresponsemanager.PeerResponseSender
}

type responseManagerMessage interface {
	handle(rm *ResponseManager)
}

// ResponseManager handles incoming requests from the network, initiates selector
// traversals, and transmits responses
type ResponseManager struct {
	ctx          context.Context
	cancelFn     context.CancelFunc
	loader       ipld.Loader
	peerManager  PeerManager
	queryQueue   QueryQueue
	requestHooks RequestHooks
	blockHooks   BlockHooks

	messages            chan responseManagerMessage
	workSignal          chan struct{}
	ticker              *time.Ticker
	inProgressResponses map[responseKey]*inProgressResponseStatus
}

// New creates a new response manager from the given context, loader,
// bridge to IPLD interface, peerManager, and queryQueue.
func New(ctx context.Context,
	loader ipld.Loader,
	peerManager PeerManager,
	queryQueue QueryQueue,
	requestHooks RequestHooks,
	blockHooks BlockHooks) *ResponseManager {
	ctx, cancelFn := context.WithCancel(ctx)
	return &ResponseManager{
		ctx:                 ctx,
		cancelFn:            cancelFn,
		loader:              loader,
		peerManager:         peerManager,
		queryQueue:          queryQueue,
		requestHooks:        requestHooks,
		blockHooks:          blockHooks,
		messages:            make(chan responseManagerMessage, 16),
		workSignal:          make(chan struct{}, 1),
		ticker:              time.NewTicker(thawSpeed),
		inProgressResponses: make(map[responseKey]*inProgressResponseStatus),
	}
}

type processRequestMessage struct {
	p        peer.ID
	requests []gsmsg.GraphSyncRequest
}

// ProcessRequests processes incoming requests for the given peer
func (rm *ResponseManager) ProcessRequests(ctx context.Context, p peer.ID, requests []gsmsg.GraphSyncRequest) {
	select {
	case rm.messages <- &processRequestMessage{p, requests}:
	case <-rm.ctx.Done():
	case <-ctx.Done():
	}
}

type unpauseRequestMessage struct {
	p         peer.ID
	requestID graphsync.RequestID
	response  chan error
}

// UnpauseResponse unpauses a response that was previously paused
func (rm *ResponseManager) UnpauseResponse(p peer.ID, requestID graphsync.RequestID) error {
	response := make(chan error, 1)
	select {
	case <-rm.ctx.Done():
		return errors.New("Context Cancelled")
	case rm.messages <- &unpauseRequestMessage{p, requestID, response}:
	}
	select {
	case <-rm.ctx.Done():
		return errors.New("Context Cancelled")
	case err := <-response:
		return err
	}
}

type synchronizeMessage struct {
	sync chan struct{}
}

// this is a test utility method to force all messages to get processed
func (rm *ResponseManager) synchronize() {
	sync := make(chan struct{})
	select {
	case rm.messages <- &synchronizeMessage{sync}:
	case <-rm.ctx.Done():
	}
	select {
	case <-sync:
	case <-rm.ctx.Done():
	}
}

type responseDataRequest struct {
	key          responseKey
	taskDataChan chan *responseTaskData
}

type finishTaskRequest struct {
	key responseKey
	err error
}

type setResponseDataRequest struct {
	key       responseKey
	loader    ipld.Loader
	traverser ipldutil.Traverser
}

func (rm *ResponseManager) processQueriesWorker() {
	const targetWork = 1
	taskDataChan := make(chan *responseTaskData)
	var taskData *responseTaskData
	for {
		pid, tasks, _ := rm.queryQueue.PopTasks(targetWork)
		for len(tasks) == 0 {
			select {
			case <-rm.ctx.Done():
				return
			case <-rm.workSignal:
				pid, tasks, _ = rm.queryQueue.PopTasks(targetWork)
			case <-rm.ticker.C:
				rm.queryQueue.ThawRound()
				pid, tasks, _ = rm.queryQueue.PopTasks(targetWork)
			}
		}
		for _, task := range tasks {
			key := task.Topic.(responseKey)
			select {
			case rm.messages <- &responseDataRequest{key, taskDataChan}:
			case <-rm.ctx.Done():
				return
			}
			select {
			case taskData = <-taskDataChan:
			case <-rm.ctx.Done():
				return
			}
			err := rm.executeTask(key, taskData)
			select {
			case rm.messages <- &finishTaskRequest{key, err}:
			case <-rm.ctx.Done():
			}
		}
		rm.queryQueue.TasksDone(pid, tasks...)

	}

}

func (rm *ResponseManager) executeTask(key responseKey, taskData *responseTaskData) error {
	var err error
	loader := taskData.loader
	traverser := taskData.traverser
	if loader == nil || traverser == nil {
		loader, traverser, err = rm.prepareQuery(taskData.ctx, key.p, taskData.request)
		if err != nil {
			return err
		}
		select {
		case <-rm.ctx.Done():
			return nil
		case rm.messages <- &setResponseDataRequest{key, loader, traverser}:
		}
	}
	return rm.executeQuery(key.p, taskData.request, loader, traverser)
}

func (rm *ResponseManager) prepareQuery(ctx context.Context,
	p peer.ID,
	request gsmsg.GraphSyncRequest) (ipld.Loader, ipldutil.Traverser, error) {
	result := rm.requestHooks.ProcessRequestHooks(p, request)
	peerResponseSender := rm.peerManager.SenderForPeer(p)
	for _, extension := range result.Extensions {
		peerResponseSender.SendExtensionData(request.ID(), extension)
	}
	if result.Err != nil || !result.IsValidated {
		peerResponseSender.FinishWithError(request.ID(), graphsync.RequestFailedUnknown)
		return nil, nil, errors.New("request not valid")
	}
	rootLink := cidlink.Link{Cid: request.Root()}
	traverser := ipldutil.TraversalBuilder{
		Root:     rootLink,
		Selector: request.Selector(),
		Chooser:  result.CustomChooser,
	}.Start(ctx)
	loader := result.CustomLoader
	if loader == nil {
		loader = rm.loader
	}
	return loader, traverser, nil
}

func (rm *ResponseManager) executeQuery(p peer.ID,
	request gsmsg.GraphSyncRequest,
	loader ipld.Loader,
	traverser ipldutil.Traverser) error {
	peerResponseSender := rm.peerManager.SenderForPeer(p)
	err := runtraversal.RunTraversal(loader, traverser, func(link ipld.Link, data []byte) error {
		blockData := peerResponseSender.SendResponse(request.ID(), link, data)
		if blockData.BlockSize() > 0 {
			result := rm.blockHooks.ProcessBlockHooks(p, request, blockData)
			for _, extension := range result.Extensions {
				peerResponseSender.SendExtensionData(request.ID(), extension)
			}
			if result.Err != nil {
				return result.Err
			}
		}
		return nil
	})
	if err != nil {
		if err != blockhooks.ErrPaused {
			peerResponseSender.FinishWithError(request.ID(), graphsync.RequestFailedUnknown)
		}
		return err
	}
	peerResponseSender.FinishRequest(request.ID())
	return nil
}

// Startup starts processing for the WantManager.
func (rm *ResponseManager) Startup() {
	go rm.run()
}

// Shutdown ends processing for the want manager.
func (rm *ResponseManager) Shutdown() {
	rm.cancelFn()
}

func (rm *ResponseManager) cleanupInProcessResponses() {
	for _, response := range rm.inProgressResponses {
		response.cancelFn()
	}
}

func (rm *ResponseManager) run() {
	defer rm.cleanupInProcessResponses()
	for i := 0; i < maxInProcessRequests; i++ {
		go rm.processQueriesWorker()
	}

	for {
		select {
		case <-rm.ctx.Done():
			return
		case message := <-rm.messages:
			message.handle(rm)
		}
	}
}

func (prm *processRequestMessage) handle(rm *ResponseManager) {
	for _, request := range prm.requests {
		key := responseKey{p: prm.p, requestID: request.ID()}
		if !request.IsCancel() {
			ctx, cancelFn := context.WithCancel(rm.ctx)
			rm.inProgressResponses[key] =
				&inProgressResponseStatus{
					ctx:      ctx,
					cancelFn: cancelFn,
					request:  request,
				}
			// TODO: Use a better work estimation metric.
			rm.queryQueue.PushTasks(prm.p, peertask.Task{Topic: key, Priority: int(request.Priority()), Work: 1})
			select {
			case rm.workSignal <- struct{}{}:
			default:
			}
		} else {
			rm.queryQueue.Remove(key, key.p)
			response, ok := rm.inProgressResponses[key]
			if ok {
				response.cancelFn()
			}
		}
	}
}

func (rdr *responseDataRequest) handle(rm *ResponseManager) {
	response, ok := rm.inProgressResponses[rdr.key]
	var taskData *responseTaskData
	if ok {
		taskData = &responseTaskData{response.ctx, response.request, response.loader, response.traverser}
	} else {
		taskData = nil
	}
	select {
	case <-rm.ctx.Done():
	case rdr.taskDataChan <- taskData:
	}
}

func (ftr *finishTaskRequest) handle(rm *ResponseManager) {
	response, ok := rm.inProgressResponses[ftr.key]
	if !ok {
		return
	}
	if ftr.err == blockhooks.ErrPaused {
		response.isPaused = true
		return
	}
	if ftr.err != nil {
		log.Infof("response failed: %w", ftr.err)
	}
	delete(rm.inProgressResponses, ftr.key)
	response.cancelFn()
}

func (srdr *setResponseDataRequest) handle(rm *ResponseManager) {
	response, ok := rm.inProgressResponses[srdr.key]
	if !ok {
		return
	}
	response.loader = srdr.loader
	response.traverser = srdr.traverser
}

func (sm *synchronizeMessage) handle(rm *ResponseManager) {
	select {
	case <-rm.ctx.Done():
	case sm.sync <- struct{}{}:
	}
}

func (urm *unpauseRequestMessage) unpauseRequest(rm *ResponseManager) error {
	key := responseKey{urm.p, urm.requestID}
	inProgressResponse, ok := rm.inProgressResponses[key]
	if !ok {
		return errors.New("could not find request")
	}
	if !inProgressResponse.isPaused {
		return errors.New("request is not paused")
	}
	inProgressResponse.isPaused = false
	rm.queryQueue.PushTasks(urm.p, peertask.Task{Topic: key, Priority: math.MaxInt32, Work: 1})
	select {
	case rm.workSignal <- struct{}{}:
	default:
	}
	return nil
}

func (urm *unpauseRequestMessage) handle(rm *ResponseManager) {
	err := urm.unpauseRequest(rm)
	select {
	case <-rm.ctx.Done():
	case urm.response <- err:
	}
}

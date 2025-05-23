// SPDX-FileCopyrightText: 2023 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

// Package ice implements the Interactive Connectivity Establishment (ICE)
// protocol defined in rfc5245.
package ice

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	atomicx "github.com/pion/ice/v2/internal/atomic"
	stunx "github.com/pion/ice/v2/internal/stun"
	"github.com/pion/logging"
	"github.com/pion/mdns"
	"github.com/pion/stun"
	"github.com/pion/transport/v2"
	"github.com/pion/transport/v2/packetio"
	"github.com/pion/transport/v2/stdnet"
	"github.com/pion/transport/v2/vnet"
	"golang.org/x/net/proxy"
)

type bindingRequest struct {
	timestamp      time.Time
	transactionID  [stun.TransactionIDSize]byte
	destination    net.Addr
	isUseCandidate bool
}

// Agent represents the ICE agent
type Agent struct {
	chanTask   chan task
	afterRunFn []func(ctx context.Context)
	muAfterRun sync.Mutex

	onConnectionStateChangeHdlr       atomic.Value // func(ConnectionState)
	onSelectedCandidatePairChangeHdlr atomic.Value // func(Candidate, Candidate)
	onCandidateHdlr                   atomic.Value // func(Candidate)

	// State owned by the taskLoop
	onConnected     chan struct{}
	onConnectedOnce sync.Once

	// Force candidate to be contacted immediately (instead of waiting for task ticker)
	forceCandidateContact chan bool

	tieBreaker uint64
	lite       bool

	connectionState ConnectionState
	gatheringState  GatheringState

	mDNSMode MulticastDNSMode
	mDNSName string
	mDNSConn *mdns.Conn

	muHaveStarted sync.Mutex
	startedCh     <-chan struct{}
	startedFn     func()
	isControlling bool

	maxBindingRequests uint16

	hostAcceptanceMinWait  time.Duration
	srflxAcceptanceMinWait time.Duration
	prflxAcceptanceMinWait time.Duration
	relayAcceptanceMinWait time.Duration

	tcpPriorityOffset uint16
	disableActiveTCP  bool

	portMin uint16
	portMax uint16

	candidateTypes []CandidateType

	// How long connectivity checks can fail before the ICE Agent
	// goes to disconnected
	disconnectedTimeout time.Duration

	// How long connectivity checks can fail before the ICE Agent
	// goes to failed
	failedTimeout time.Duration

	// How often should we send keepalive packets?
	// 0 means never
	keepaliveInterval time.Duration

	// How often should we run our internal taskLoop to check for state changes when connecting
	checkInterval time.Duration

	localUfrag      string
	localPwd        string
	localCandidates map[NetworkType][]Candidate

	remoteUfrag      string
	remotePwd        string
	remoteCandidates map[NetworkType][]Candidate

	checklist []*CandidatePair
	selector  pairCandidateSelector

	selectedPair atomic.Value // *CandidatePair

	urls         []*stun.URI
	networkTypes []NetworkType

	buf *packetio.Buffer

	// LRU of outbound Binding request Transaction IDs
	pendingBindingRequests []bindingRequest

	// 1:1 D-NAT IP address mapping
	extIPMapper *externalIPMapper

	// State for closing
	done         chan struct{}
	taskLoopDone chan struct{}
	err          atomicx.Error

	// Callback that allows user to implement custom behavior
	// for STUN Binding Requests
	userBindingRequestHandler func(m *stun.Message, local, remote Candidate, pair *CandidatePair) bool

	gatherCandidateCancel func()
	gatherCandidateDone   chan struct{}

	connectionStateNotifier       *handlerNotifier
	candidateNotifier             *handlerNotifier
	selectedCandidatePairNotifier *handlerNotifier

	loggerFactory logging.LoggerFactory
	log           logging.LeveledLogger

	net         transport.Net
	tcpMux      TCPMux
	udpMux      UDPMux
	udpMuxSrflx UniversalUDPMux

	interfaceFilter func(string) bool
	ipFilter        func(net.IP) bool
	includeLoopback bool

	insecureSkipVerify bool

	proxyDialer proxy.Dialer

	enableUseCandidateCheckPriority bool
}

type task struct {
	fn   func(context.Context, *Agent)
	done chan struct{}
}

// afterRun registers function to be run after the task.
func (a *Agent) afterRun(f func(context.Context)) {
	a.muAfterRun.Lock()
	a.afterRunFn = append(a.afterRunFn, f)
	a.muAfterRun.Unlock()
}

func (a *Agent) getAfterRunFn() []func(context.Context) {
	a.muAfterRun.Lock()
	defer a.muAfterRun.Unlock()
	fns := a.afterRunFn
	a.afterRunFn = nil
	return fns
}

func (a *Agent) ok() error {
	select {
	case <-a.done:
		return a.getErr()
	default:
	}
	return nil
}

func (a *Agent) getErr() error {
	if err := a.err.Load(); err != nil {
		return err
	}
	return ErrClosed
}

// Run task in serial. Blocking tasks must be cancelable by context.
func (a *Agent) run(ctx context.Context, t func(context.Context, *Agent)) error {
	if err := a.ok(); err != nil {
		return err
	}
	done := make(chan struct{})
	select {
	case <-ctx.Done():
		return ctx.Err()
	case a.chanTask <- task{t, done}:
		<-done
		return nil
	}
}

// taskLoop handles registered tasks and agent close.
func (a *Agent) taskLoop() {
	after := func() {
		for {
			// Get and run func registered by afterRun().
			fns := a.getAfterRunFn()
			if len(fns) == 0 {
				break
			}
			for _, fn := range fns {
				fn(a.context())
			}
		}
	}
	defer func() {
		a.deleteAllCandidates()
		a.startedFn()

		if err := a.buf.Close(); err != nil {
			a.log.Warnf("Failed to close buffer: %v", err)
		}

		a.closeMulticastConn()
		a.updateConnectionState(ConnectionStateClosed)

		after()

		close(a.taskLoopDone)
	}()

	for {
		select {
		case <-a.done:
			return
		case t := <-a.chanTask:
			t.fn(a.context(), a)
			close(t.done)
			after()
		}
	}
}

// NewAgent creates a new Agent
func NewAgent(config *AgentConfig) (*Agent, error) { //nolint:gocognit
	var err error
	if config.PortMax < config.PortMin {
		return nil, ErrPort
	}

	mDNSName := config.MulticastDNSHostName
	if mDNSName == "" {
		if mDNSName, err = generateMulticastDNSName(); err != nil {
			return nil, err
		}
	}

	if !strings.HasSuffix(mDNSName, ".local") || len(strings.Split(mDNSName, ".")) != 2 {
		return nil, ErrInvalidMulticastDNSHostName
	}

	mDNSMode := config.MulticastDNSMode
	if mDNSMode == 0 {
		mDNSMode = MulticastDNSModeQueryOnly
	}

	loggerFactory := config.LoggerFactory
	if loggerFactory == nil {
		loggerFactory = logging.NewDefaultLoggerFactory()
	}
	log := loggerFactory.NewLogger("ice")

	startedCtx, startedFn := context.WithCancel(context.Background())

	a := &Agent{
		chanTask:         make(chan task),
		tieBreaker:       globalMathRandomGenerator.Uint64(),
		lite:             config.Lite,
		gatheringState:   GatheringStateNew,
		connectionState:  ConnectionStateNew,
		localCandidates:  make(map[NetworkType][]Candidate),
		remoteCandidates: make(map[NetworkType][]Candidate),
		urls:             config.Urls,
		networkTypes:     config.NetworkTypes,
		onConnected:      make(chan struct{}),
		buf:              packetio.NewBuffer(),
		done:             make(chan struct{}),
		taskLoopDone:     make(chan struct{}),
		startedCh:        startedCtx.Done(),
		startedFn:        startedFn,
		portMin:          config.PortMin,
		portMax:          config.PortMax,
		loggerFactory:    loggerFactory,
		log:              log,
		net:              config.Net,
		proxyDialer:      config.ProxyDialer,
		tcpMux:           config.TCPMux,
		udpMux:           config.UDPMux,
		udpMuxSrflx:      config.UDPMuxSrflx,

		mDNSMode: mDNSMode,
		mDNSName: mDNSName,

		gatherCandidateCancel: func() {},

		forceCandidateContact: make(chan bool, 1),

		interfaceFilter: config.InterfaceFilter,

		ipFilter: config.IPFilter,

		insecureSkipVerify: config.InsecureSkipVerify,

		includeLoopback: config.IncludeLoopback,

		disableActiveTCP: config.DisableActiveTCP,

		userBindingRequestHandler: config.BindingRequestHandler,

		enableUseCandidateCheckPriority: config.EnableUseCandidateCheckPriority,
	}
	a.connectionStateNotifier = &handlerNotifier{connectionStateFunc: a.onConnectionStateChange, done: make(chan struct{})}
	a.candidateNotifier = &handlerNotifier{candidateFunc: a.onCandidate, done: make(chan struct{})}
	a.selectedCandidatePairNotifier = &handlerNotifier{candidatePairFunc: a.onSelectedCandidatePairChange, done: make(chan struct{})}

	if a.net == nil {
		a.net, err = stdnet.NewNet()
		if err != nil {
			return nil, fmt.Errorf("failed to create network: %w", err)
		}
	} else if _, isVirtual := a.net.(*vnet.Net); isVirtual {
		a.log.Warn("Virtual network is enabled")
		if a.mDNSMode != MulticastDNSModeDisabled {
			a.log.Warn("Virtual network does not support mDNS yet")
		}
	}

	// Opportunistic mDNS: If we can't open the connection, that's ok: we
	// can continue without it.
	if a.mDNSConn, a.mDNSMode, err = createMulticastDNS(a.net, mDNSMode, mDNSName, log); err != nil {
		log.Warnf("Failed to initialize mDNS %s: %v", mDNSName, err)
	}

	config.initWithDefaults(a)

	// Make sure the buffer doesn't grow indefinitely.
	// NOTE: We actually won't get anywhere close to this limit.
	// SRTP will constantly read from the endpoint and drop packets if it's full.
	a.buf.SetLimitSize(maxBufferSize)

	if a.lite && (len(a.candidateTypes) != 1 || a.candidateTypes[0] != CandidateTypeHost) {
		a.closeMulticastConn()
		return nil, ErrLiteUsingNonHostCandidates
	}

	if config.Urls != nil && len(config.Urls) > 0 && !containsCandidateType(CandidateTypeServerReflexive, a.candidateTypes) && !containsCandidateType(CandidateTypeRelay, a.candidateTypes) {
		a.closeMulticastConn()
		return nil, ErrUselessUrlsProvided
	}

	if err = config.initExtIPMapping(a); err != nil {
		a.closeMulticastConn()
		return nil, err
	}

	go a.taskLoop()

	// Restart is also used to initialize the agent for the first time
	if err := a.Restart(config.LocalUfrag, config.LocalPwd); err != nil {
		a.closeMulticastConn()
		_ = a.Close()
		return nil, err
	}

	return a, nil
}

func (a *Agent) startConnectivityChecks(isControlling bool, remoteUfrag, remotePwd string) error {
	a.muHaveStarted.Lock()
	defer a.muHaveStarted.Unlock()
	select {
	case <-a.startedCh:
		return ErrMultipleStart
	default:
	}
	if err := a.SetRemoteCredentials(remoteUfrag, remotePwd); err != nil { //nolint:contextcheck
		return err
	}

	a.log.Debugf("Started agent: isControlling? %t, remoteUfrag: %q, remotePwd: %q", isControlling, remoteUfrag, remotePwd)

	return a.run(a.context(), func(ctx context.Context, agent *Agent) {
		agent.isControlling = isControlling
		agent.remoteUfrag = remoteUfrag
		agent.remotePwd = remotePwd

		if isControlling {
			a.selector = &controllingSelector{agent: a, log: a.log}
		} else {
			a.selector = &controlledSelector{agent: a, log: a.log}
		}

		if a.lite {
			a.selector = &liteSelector{pairCandidateSelector: a.selector}
		}

		a.selector.Start()
		a.startedFn()

		agent.updateConnectionState(ConnectionStateChecking)

		a.requestConnectivityCheck()
		go a.connectivityChecks() //nolint:contextcheck
	})
}

func (a *Agent) connectivityChecks() {
	lastConnectionState := ConnectionState(0)
	checkingDuration := time.Time{}

	contact := func() {
		if err := a.run(a.context(), func(ctx context.Context, a *Agent) {
			defer func() {
				lastConnectionState = a.connectionState
			}()

			switch a.connectionState {
			case ConnectionStateFailed:
				// The connection is currently failed so don't send any checks
				// In the future it may be restarted though
				return
			case ConnectionStateChecking:
				// We have just entered checking for the first time so update our checking timer
				if lastConnectionState != a.connectionState {
					checkingDuration = time.Now()
				}

				// We have been in checking longer then Disconnect+Failed timeout, set the connection to Failed
				if time.Since(checkingDuration) > a.disconnectedTimeout+a.failedTimeout {
					a.updateConnectionState(ConnectionStateFailed)
					return
				}
			default:
			}

			a.selector.ContactCandidates()
		}); err != nil {
			a.log.Warnf("Failed to start connectivity checks: %v", err)
		}
	}

	t := time.NewTimer(math.MaxInt64)
	t.Stop()

	for {
		interval := defaultKeepaliveInterval

		updateInterval := func(x time.Duration) {
			if x != 0 && (interval == 0 || interval > x) {
				interval = x
			}
		}

		switch lastConnectionState {
		case ConnectionStateNew, ConnectionStateChecking: // While connecting, check candidates more frequently
			updateInterval(a.checkInterval)
		case ConnectionStateConnected, ConnectionStateDisconnected:
			updateInterval(a.keepaliveInterval)
		default:
		}
		// Ensure we run our task loop as quickly as the minimum of our various configured timeouts
		updateInterval(a.disconnectedTimeout)
		updateInterval(a.failedTimeout)

		t.Reset(interval)

		select {
		case <-a.forceCandidateContact:
			if !t.Stop() {
				<-t.C
			}
			contact()
		case <-t.C:
			contact()
		case <-a.done:
			t.Stop()
			return
		}
	}
}

func (a *Agent) updateConnectionState(newState ConnectionState) {
	if a.connectionState != newState {
		// Connection has gone to failed, release all gathered candidates
		if newState == ConnectionStateFailed {
			a.removeUfragFromMux()
			a.checklist = make([]*CandidatePair, 0)
			a.pendingBindingRequests = make([]bindingRequest, 0)
			a.setSelectedPair(nil)
			a.deleteAllCandidates()
		}

		a.log.Infof("Setting new connection state: %s", newState)
		a.connectionState = newState
		a.connectionStateNotifier.EnqueueConnectionState(newState)
	}
}

func (a *Agent) setSelectedPair(p *CandidatePair) {
	if p == nil {
		var nilPair *CandidatePair
		a.selectedPair.Store(nilPair)
		a.log.Tracef("Unset selected candidate pair")
		return
	}

	p.nominated = true
	a.selectedPair.Store(p)
	a.log.Tracef("Set selected candidate pair: %s", p)

	a.updateConnectionState(ConnectionStateConnected)

	// Notify when the selected pair changes
	a.selectedCandidatePairNotifier.EnqueueSelectedCandidatePair(p)

	// Signal connected
	a.onConnectedOnce.Do(func() { close(a.onConnected) })
}

func (a *Agent) pingAllCandidates() {
	a.log.Trace("Pinging all candidates")

	if len(a.checklist) == 0 {
		a.log.Warn("Failed to ping without candidate pairs. Connection is not possible yet.")
	}

	for _, p := range a.checklist {
		if p.state == CandidatePairStateWaiting {
			p.state = CandidatePairStateInProgress
		} else if p.state != CandidatePairStateInProgress {
			continue
		}

		if p.bindingRequestCount > a.maxBindingRequests {
			a.log.Tracef("Maximum requests reached for pair %s, marking it as failed", p)
			p.state = CandidatePairStateFailed
		} else {
			a.selector.PingCandidate(p.Local, p.Remote)
			p.bindingRequestCount++
		}
	}
}

func (a *Agent) getBestAvailableCandidatePair() *CandidatePair {
	var best *CandidatePair
	for _, p := range a.checklist {
		if p.state == CandidatePairStateFailed {
			continue
		}

		if best == nil {
			best = p
		} else if best.priority() < p.priority() {
			best = p
		}
	}
	return best
}

func (a *Agent) getBestValidCandidatePair() *CandidatePair {
	var best *CandidatePair
	for _, p := range a.checklist {
		if p.state != CandidatePairStateSucceeded {
			continue
		}

		if best == nil {
			best = p
		} else if best.priority() < p.priority() {
			best = p
		}
	}
	return best
}

func (a *Agent) addPair(local, remote Candidate) *CandidatePair {
	p := newCandidatePair(local, remote, a.isControlling)
	a.checklist = append(a.checklist, p)
	return p
}

func (a *Agent) findPair(local, remote Candidate) *CandidatePair {
	for _, p := range a.checklist {
		if p.Local.Equal(local) && p.Remote.Equal(remote) {
			return p
		}
	}
	return nil
}

// validateSelectedPair checks if the selected pair is (still) valid
// Note: the caller should hold the agent lock.
func (a *Agent) validateSelectedPair() bool {
	selectedPair := a.getSelectedPair()
	if selectedPair == nil {
		return false
	}

	disconnectedTime := time.Since(selectedPair.Remote.LastReceived())

	// Only allow transitions to failed if a.failedTimeout is non-zero
	totalTimeToFailure := a.failedTimeout
	if totalTimeToFailure != 0 {
		totalTimeToFailure += a.disconnectedTimeout
	}

	switch {
	case totalTimeToFailure != 0 && disconnectedTime > totalTimeToFailure:
		a.updateConnectionState(ConnectionStateFailed)
	case a.disconnectedTimeout != 0 && disconnectedTime > a.disconnectedTimeout:
		a.updateConnectionState(ConnectionStateDisconnected)
	default:
		a.updateConnectionState(ConnectionStateConnected)
	}

	return true
}

// checkKeepalive sends STUN Binding Indications to the selected pair
// if no packet has been sent on that pair in the last keepaliveInterval
// Note: the caller should hold the agent lock.
func (a *Agent) checkKeepalive() {
	selectedPair := a.getSelectedPair()
	if selectedPair == nil {
		return
	}

	if (a.keepaliveInterval != 0) &&
		((time.Since(selectedPair.Local.LastSent()) > a.keepaliveInterval) ||
			(time.Since(selectedPair.Remote.LastReceived()) > a.keepaliveInterval)) {
		// We use binding request instead of indication to support refresh consent schemas
		// see https://tools.ietf.org/html/rfc7675
		a.selector.PingCandidate(selectedPair.Local, selectedPair.Remote)
	}
}

// AddRemoteCandidate adds a new remote candidate
func (a *Agent) AddRemoteCandidate(c Candidate) error {
	if c == nil {
		return nil
	}

	// TCP Candidates with TCP type active will probe server passive ones, so
	// no need to do anything with them.
	if c.TCPType() == TCPTypeActive {
		a.log.Infof("Ignoring remote candidate with tcpType active: %s", c)
		return nil
	}

	// If we have a mDNS Candidate lets fully resolve it before adding it locally
	if c.Type() == CandidateTypeHost && strings.HasSuffix(c.Address(), ".local") {
		if a.mDNSMode == MulticastDNSModeDisabled {
			a.log.Warnf("Remote mDNS candidate added, but mDNS is disabled: (%s)", c.Address())
			return nil
		}

		hostCandidate, ok := c.(*CandidateHost)
		if !ok {
			return ErrAddressParseFailed
		}

		go a.resolveAndAddMulticastCandidate(hostCandidate)
		return nil
	}

	go func() {
		if err := a.run(a.context(), func(ctx context.Context, agent *Agent) {
			// nolint: contextcheck
			agent.addRemoteCandidate(c)
		}); err != nil {
			a.log.Warnf("Failed to add remote candidate %s: %v", c.Address(), err)
			return
		}
	}()
	return nil
}

func (a *Agent) resolveAndAddMulticastCandidate(c *CandidateHost) {
	if a.mDNSConn == nil {
		return
	}
	_, src, err := a.mDNSConn.Query(c.context(), c.Address())
	if err != nil {
		a.log.Warnf("Failed to discover mDNS candidate %s: %v", c.Address(), err)
		return
	}

	ip, ipOk := parseMulticastAnswerAddr(src)
	if !ipOk {
		a.log.Warnf("Failed to discover mDNS candidate %s: failed to parse IP", c.Address())
		return
	}

	if err = c.setIP(ip); err != nil {
		a.log.Warnf("Failed to discover mDNS candidate %s: %v", c.Address(), err)
		return
	}

	if err = a.run(a.context(), func(ctx context.Context, agent *Agent) {
		// nolint: contextcheck
		agent.addRemoteCandidate(c)
	}); err != nil {
		a.log.Warnf("Failed to add mDNS candidate %s: %v", c.Address(), err)
		return
	}
}

func (a *Agent) requestConnectivityCheck() {
	select {
	case a.forceCandidateContact <- true:
	default:
	}
}

func (a *Agent) addRemotePassiveTCPCandidate(remoteCandidate Candidate) {
	localIPs, err := localInterfaces(a.net, a.interfaceFilter, a.ipFilter, []NetworkType{remoteCandidate.NetworkType()}, a.includeLoopback)
	if err != nil {
		a.log.Warnf("Failed to iterate local interfaces, host candidates will not be gathered %s", err)
		return
	}

	for i := range localIPs {
		conn := newActiveTCPConn(
			a.context(),
			net.JoinHostPort(localIPs[i].String(), "0"),
			net.JoinHostPort(remoteCandidate.Address(), strconv.Itoa(remoteCandidate.Port())),
			a.log,
		)

		tcpAddr, ok := conn.LocalAddr().(*net.TCPAddr)
		if !ok {
			closeConnAndLog(conn, a.log, "Failed to create Active ICE-TCP Candidate: %v", errInvalidAddress)
			continue
		}

		localCandidate, err := NewCandidateHost(&CandidateHostConfig{
			Network:   remoteCandidate.NetworkType().String(),
			Address:   localIPs[i].String(),
			Port:      tcpAddr.Port,
			Component: ComponentRTP,
			TCPType:   TCPTypeActive,
		})
		if err != nil {
			closeConnAndLog(conn, a.log, "Failed to create Active ICE-TCP Candidate: %v", err)
			continue
		}

		localCandidate.start(a, conn, a.startedCh)
		a.localCandidates[localCandidate.NetworkType()] = append(a.localCandidates[localCandidate.NetworkType()], localCandidate)
		a.candidateNotifier.EnqueueCandidate(localCandidate)

		a.addPair(localCandidate, remoteCandidate)
	}
}

// addRemoteCandidate assumes you are holding the lock (must be execute using a.run)
func (a *Agent) addRemoteCandidate(c Candidate) {
	set := a.remoteCandidates[c.NetworkType()]

	for _, candidate := range set {
		if candidate.Equal(c) {
			return
		}
	}

	tcpNetworkTypeFound := false
	for _, networkType := range a.networkTypes {
		if networkType.IsTCP() {
			tcpNetworkTypeFound = true
		}
	}

	if !a.disableActiveTCP && tcpNetworkTypeFound && c.TCPType() == TCPTypePassive {
		a.addRemotePassiveTCPCandidate(c)
	}

	set = append(set, c)
	a.remoteCandidates[c.NetworkType()] = set

	if c.TCPType() != TCPTypePassive {
		if localCandidates, ok := a.localCandidates[c.NetworkType()]; ok {
			for _, localCandidate := range localCandidates {
				a.addPair(localCandidate, c)
			}
		}
	}

	a.requestConnectivityCheck()
}

func (a *Agent) addCandidate(ctx context.Context, c Candidate, candidateConn net.PacketConn) error {
	return a.run(ctx, func(ctx context.Context, agent *Agent) {
		set := a.localCandidates[c.NetworkType()]
		for _, candidate := range set {
			if candidate.Equal(c) {
				a.log.Debugf("Ignore duplicate candidate: %s", c)
				if err := c.close(); err != nil {
					a.log.Warnf("Failed to close duplicate candidate: %v", err)
				}
				if err := candidateConn.Close(); err != nil {
					a.log.Warnf("Failed to close duplicate candidate connection: %v", err)
				}
				return
			}
		}

		c.start(a, candidateConn, a.startedCh)

		set = append(set, c)
		a.localCandidates[c.NetworkType()] = set

		if remoteCandidates, ok := a.remoteCandidates[c.NetworkType()]; ok {
			for _, remoteCandidate := range remoteCandidates {
				a.addPair(c, remoteCandidate)
			}
		}

		a.requestConnectivityCheck()

		a.candidateNotifier.EnqueueCandidate(c)
	})
}

// GetRemoteCandidates returns the remote candidates
func (a *Agent) GetRemoteCandidates() ([]Candidate, error) {
	var res []Candidate

	err := a.run(a.context(), func(ctx context.Context, agent *Agent) {
		var candidates []Candidate
		for _, set := range agent.remoteCandidates {
			candidates = append(candidates, set...)
		}
		res = candidates
	})
	if err != nil {
		return nil, err
	}

	return res, nil
}

// GetLocalCandidates returns the local candidates
func (a *Agent) GetLocalCandidates() ([]Candidate, error) {
	var res []Candidate

	err := a.run(a.context(), func(ctx context.Context, agent *Agent) {
		var candidates []Candidate
		for _, set := range agent.localCandidates {
			candidates = append(candidates, set...)
		}
		res = candidates
	})
	if err != nil {
		return nil, err
	}

	return res, nil
}

// GetLocalUserCredentials returns the local user credentials
func (a *Agent) GetLocalUserCredentials() (frag string, pwd string, err error) {
	valSet := make(chan struct{})
	err = a.run(a.context(), func(ctx context.Context, agent *Agent) {
		frag = agent.localUfrag
		pwd = agent.localPwd
		close(valSet)
	})

	if err == nil {
		<-valSet
	}
	return
}

// GetRemoteUserCredentials returns the remote user credentials
func (a *Agent) GetRemoteUserCredentials() (frag string, pwd string, err error) {
	valSet := make(chan struct{})
	err = a.run(a.context(), func(ctx context.Context, agent *Agent) {
		frag = agent.remoteUfrag
		pwd = agent.remotePwd
		close(valSet)
	})

	if err == nil {
		<-valSet
	}
	return
}

func (a *Agent) removeUfragFromMux() {
	if a.tcpMux != nil {
		a.tcpMux.RemoveConnByUfrag(a.localUfrag)
	}
	if a.udpMux != nil {
		a.udpMux.RemoveConnByUfrag(a.localUfrag)
	}
	if a.udpMuxSrflx != nil {
		a.udpMuxSrflx.RemoveConnByUfrag(a.localUfrag)
	}
}

// Close cleans up the Agent
func (a *Agent) Close() error {
	return a.close(false)
}

// GracefulClose cleans up the Agent and waits for any goroutines it started
// to complete. This is only safe to call outside of Agent callbacks or if in a callback,
// in its own goroutine.
func (a *Agent) GracefulClose() error {
	return a.close(true)
}

func (a *Agent) close(graceful bool) error {
	if err := a.ok(); err != nil {
		if errors.Is(err, ErrClosed) {
			return nil
		}
		return err
	}

	a.afterRun(func(context.Context) {
		a.gatherCandidateCancel()
		if a.gatherCandidateDone != nil {
			<-a.gatherCandidateDone
		}
	})
	a.err.Store(ErrClosed)

	a.removeUfragFromMux()

	close(a.done)
	// the loop is safe to wait on no matter what
	<-a.taskLoopDone

	// but we are in less control of the notifiers, so we will
	// pass through `graceful`.
	a.connectionStateNotifier.Close(graceful)
	a.candidateNotifier.Close(graceful)
	a.selectedCandidatePairNotifier.Close(graceful)
	return nil
}

// Remove all candidates. This closes any listening sockets
// and removes both the local and remote candidate lists.
//
// This is used for restarts, failures and on close
func (a *Agent) deleteAllCandidates() {
	for net, cs := range a.localCandidates {
		for _, c := range cs {
			if err := c.close(); err != nil {
				a.log.Warnf("Failed to close candidate %s: %v", c, err)
			}
		}
		delete(a.localCandidates, net)
	}
	for net, cs := range a.remoteCandidates {
		for _, c := range cs {
			if err := c.close(); err != nil {
				a.log.Warnf("Failed to close candidate %s: %v", c, err)
			}
		}
		delete(a.remoteCandidates, net)
	}
}

func (a *Agent) findRemoteCandidate(networkType NetworkType, addr net.Addr) Candidate {
	ip, port, _, ok := parseAddr(addr)
	if !ok {
		a.log.Warnf("Failed to parse address: %s", addr)
		return nil
	}

	set := a.remoteCandidates[networkType]
	for _, c := range set {
		if c.Address() == ip.String() && c.Port() == port {
			return c
		}
	}
	return nil
}

func (a *Agent) sendBindingRequest(m *stun.Message, local, remote Candidate) {
	a.log.Tracef("Ping STUN from %s to %s", local, remote)

	a.invalidatePendingBindingRequests(time.Now())
	a.pendingBindingRequests = append(a.pendingBindingRequests, bindingRequest{
		timestamp:      time.Now(),
		transactionID:  m.TransactionID,
		destination:    remote.addr(),
		isUseCandidate: m.Contains(stun.AttrUseCandidate),
	})

	a.sendSTUN(m, local, remote)
}

func (a *Agent) sendBindingSuccess(m *stun.Message, local, remote Candidate) {
	base := remote

	ip, port, _, ok := parseAddr(base.addr())
	if !ok {
		a.log.Warnf("Failed to parse address: %s", base.addr())
		return
	}

	if out, err := stun.Build(m, stun.BindingSuccess,
		&stun.XORMappedAddress{
			IP:   ip,
			Port: port,
		},
		stun.NewShortTermIntegrity(a.localPwd),
		stun.Fingerprint,
	); err != nil {
		a.log.Warnf("Failed to handle inbound ICE from: %s to: %s error: %s", local, remote, err)
	} else {
		a.sendSTUN(out, local, remote)
	}
}

// Removes pending binding requests that are over maxBindingRequestTimeout old
//
// Let HTO be the transaction timeout, which SHOULD be 2*RTT if
// RTT is known or 500 ms otherwise.
// https://tools.ietf.org/html/rfc8445#appendix-B.1
func (a *Agent) invalidatePendingBindingRequests(filterTime time.Time) {
	initialSize := len(a.pendingBindingRequests)

	temp := a.pendingBindingRequests[:0]
	for _, bindingRequest := range a.pendingBindingRequests {
		if filterTime.Sub(bindingRequest.timestamp) < maxBindingRequestTimeout {
			temp = append(temp, bindingRequest)
		}
	}

	a.pendingBindingRequests = temp
	if bindRequestsRemoved := initialSize - len(a.pendingBindingRequests); bindRequestsRemoved > 0 {
		a.log.Tracef("Discarded %d binding requests because they expired", bindRequestsRemoved)
	}
}

// Assert that the passed TransactionID is in our pendingBindingRequests and returns the destination
// If the bindingRequest was valid remove it from our pending cache
func (a *Agent) handleInboundBindingSuccess(id [stun.TransactionIDSize]byte) (bool, *bindingRequest, time.Duration) {
	a.invalidatePendingBindingRequests(time.Now())
	for i := range a.pendingBindingRequests {
		if a.pendingBindingRequests[i].transactionID == id {
			validBindingRequest := a.pendingBindingRequests[i]
			a.pendingBindingRequests = append(a.pendingBindingRequests[:i], a.pendingBindingRequests[i+1:]...)
			return true, &validBindingRequest, time.Since(validBindingRequest.timestamp)
		}
	}
	return false, nil, 0
}

// handleInbound processes STUN traffic from a remote candidate
func (a *Agent) handleInbound(m *stun.Message, local Candidate, remote net.Addr) { //nolint:gocognit
	var err error
	if m == nil || local == nil {
		return
	}

	if m.Type.Method != stun.MethodBinding ||
		!(m.Type.Class == stun.ClassSuccessResponse ||
			m.Type.Class == stun.ClassRequest ||
			m.Type.Class == stun.ClassIndication) {
		a.log.Tracef("Unhandled STUN from %s to %s class(%s) method(%s)", remote, local, m.Type.Class, m.Type.Method)
		return
	}

	if a.isControlling {
		if m.Contains(stun.AttrICEControlling) {
			a.log.Debug("Inbound STUN message: isControlling && a.isControlling == true")
			return
		} else if m.Contains(stun.AttrUseCandidate) {
			a.log.Debug("Inbound STUN message: useCandidate && a.isControlling == true")
			return
		}
	} else {
		if m.Contains(stun.AttrICEControlled) {
			a.log.Debug("Inbound STUN message: isControlled && a.isControlling == false")
			return
		}
	}

	remoteCandidate := a.findRemoteCandidate(local.NetworkType(), remote)
	if m.Type.Class == stun.ClassSuccessResponse {
		if err = stun.MessageIntegrity([]byte(a.remotePwd)).Check(m); err != nil {
			a.log.Warnf("Discard message from (%s), %v", remote, err)
			return
		}

		if remoteCandidate == nil {
			a.log.Warnf("Discard success message from (%s), no such remote", remote)
			return
		}

		a.selector.HandleSuccessResponse(m, local, remoteCandidate, remote)
	} else if m.Type.Class == stun.ClassRequest {
		a.log.Tracef("Inbound STUN (Request) from %s to %s, useCandidate: %v", remote, local, m.Contains(stun.AttrUseCandidate))

		if err = stunx.AssertUsername(m, a.localUfrag+":"+a.remoteUfrag); err != nil {
			a.log.Warnf("Discard message from (%s), %v", remote, err)
			return
		} else if err = stun.MessageIntegrity([]byte(a.localPwd)).Check(m); err != nil {
			a.log.Warnf("Discard message from (%s), %v", remote, err)
			return
		}

		if remoteCandidate == nil {
			ip, port, networkType, ok := parseAddr(remote)
			if !ok {
				a.log.Errorf("Failed to create parse remote net.Addr when creating remote prflx candidate")
				return
			}

			prflxCandidateConfig := CandidatePeerReflexiveConfig{
				Network:   networkType.String(),
				Address:   ip.String(),
				Port:      port,
				Component: local.Component(),
				RelAddr:   "",
				RelPort:   0,
			}

			prflxCandidate, err := NewCandidatePeerReflexive(&prflxCandidateConfig)
			if err != nil {
				a.log.Errorf("Failed to create new remote prflx candidate (%s)", err)
				return
			}
			remoteCandidate = prflxCandidate

			a.log.Debugf("Adding a new peer-reflexive candidate: %s ", remote)
			a.addRemoteCandidate(remoteCandidate)
		}

		a.selector.HandleBindingRequest(m, local, remoteCandidate)
	}

	if remoteCandidate != nil {
		remoteCandidate.seen(false)
	}
}

// validateNonSTUNTraffic processes non STUN traffic from a remote candidate,
// and returns true if it is an actual remote candidate
func (a *Agent) validateNonSTUNTraffic(local Candidate, remote net.Addr) (Candidate, bool) {
	var remoteCandidate Candidate
	if err := a.run(local.context(), func(ctx context.Context, agent *Agent) {
		remoteCandidate = a.findRemoteCandidate(local.NetworkType(), remote)
		if remoteCandidate != nil {
			remoteCandidate.seen(false)
		}
	}); err != nil {
		a.log.Warnf("Failed to validate remote candidate: %v", err)
	}

	return remoteCandidate, remoteCandidate != nil
}

// GetSelectedCandidatePair returns the selected pair or nil if there is none
func (a *Agent) GetSelectedCandidatePair() (*CandidatePair, error) {
	selectedPair := a.getSelectedPair()
	if selectedPair == nil {
		return nil, nil //nolint:nilnil
	}

	local, err := selectedPair.Local.copy()
	if err != nil {
		return nil, err
	}

	remote, err := selectedPair.Remote.copy()
	if err != nil {
		return nil, err
	}

	return &CandidatePair{Local: local, Remote: remote}, nil
}

func (a *Agent) getSelectedPair() *CandidatePair {
	if selectedPair, ok := a.selectedPair.Load().(*CandidatePair); ok {
		return selectedPair
	}

	return nil
}

func (a *Agent) closeMulticastConn() {
	if a.mDNSConn != nil {
		if err := a.mDNSConn.Close(); err != nil {
			a.log.Warnf("Failed to close mDNS Conn: %v", err)
		}
	}
}

// SetRemoteCredentials sets the credentials of the remote agent
func (a *Agent) SetRemoteCredentials(remoteUfrag, remotePwd string) error {
	switch {
	case remoteUfrag == "":
		return ErrRemoteUfragEmpty
	case remotePwd == "":
		return ErrRemotePwdEmpty
	}

	return a.run(a.context(), func(ctx context.Context, agent *Agent) {
		agent.remoteUfrag = remoteUfrag
		agent.remotePwd = remotePwd
	})
}

// Restart restarts the ICE Agent with the provided ufrag/pwd
// If no ufrag/pwd is provided the Agent will generate one itself
//
// If there is a gatherer routine currently running, Restart will
// cancel it.
// After a Restart, the user must then call GatherCandidates explicitly
// to start generating new ones.
func (a *Agent) Restart(ufrag, pwd string) error {
	if ufrag == "" {
		var err error
		ufrag, err = generateUFrag()
		if err != nil {
			return err
		}
	}
	if pwd == "" {
		var err error
		pwd, err = generatePwd()
		if err != nil {
			return err
		}
	}

	if len([]rune(ufrag))*8 < 24 {
		return ErrLocalUfragInsufficientBits
	}
	if len([]rune(pwd))*8 < 128 {
		return ErrLocalPwdInsufficientBits
	}

	var err error
	if runErr := a.run(a.context(), func(ctx context.Context, agent *Agent) {
		if agent.gatheringState == GatheringStateGathering {
			agent.gatherCandidateCancel()
		}

		// Clear all agent needed to take back to fresh state
		a.removeUfragFromMux()
		agent.localUfrag = ufrag
		agent.localPwd = pwd
		agent.remoteUfrag = ""
		agent.remotePwd = ""
		a.gatheringState = GatheringStateNew
		a.checklist = make([]*CandidatePair, 0)
		a.pendingBindingRequests = make([]bindingRequest, 0)
		a.setSelectedPair(nil)
		a.deleteAllCandidates()
		if a.selector != nil {
			a.selector.Start()
		}

		// Restart is used by NewAgent. Accept/Connect should be used to move to checking
		// for new Agents
		if a.connectionState != ConnectionStateNew {
			a.updateConnectionState(ConnectionStateChecking)
		}
	}); runErr != nil {
		return runErr
	}
	return err
}

func (a *Agent) setGatheringState(newState GatheringState) error {
	done := make(chan struct{})
	if err := a.run(a.context(), func(ctx context.Context, agent *Agent) {
		if a.gatheringState != newState && newState == GatheringStateComplete {
			a.candidateNotifier.EnqueueCandidate(nil)
		}

		a.gatheringState = newState
		close(done)
	}); err != nil {
		return err
	}

	<-done
	return nil
}

func (a *Agent) needsToCheckPriorityOnNominated() bool {
	return !a.lite || a.enableUseCandidateCheckPriority
}

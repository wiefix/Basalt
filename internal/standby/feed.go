package standby

import (
	"bufio"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/auth"
	"github.com/wiefix/basalt/internal/pki"
	"github.com/wiefix/basalt/internal/store"
	"github.com/wiefix/basalt/internal/walship"
)

const (
	maxEnrollBody = 64 << 10
	writeTimeout  = 30 * time.Second
	seenEvery     = time.Minute
)

// State is how a standby panel's copy goes, as the panel sees it.
type State struct {
	Connected bool
	Since     time.Time // connected since
	Report    Report    // its last report
	ReportAt  time.Time
}

// Feeder serves standby panels on the agents' port: enrollment,
// certificate renewal and the stream of the database.
type Feeder struct {
	db        *store.DB
	ca        *pki.CA
	src       *walship.Source
	masterKey []byte
	log       *slog.Logger
	now       func() time.Time
	limiter   *auth.Limiter

	// onAgentURL runs when a standby's agent address changes.
	onAgentURL func(ctx context.Context)
	hub        PanelHub

	mu     sync.Mutex
	states map[int64]*State
	cancel map[int64]context.CancelFunc
	// kick tells a stream to send the panel's addresses again.
	kick map[int64]chan struct{}
}

// PanelHub knows the panel's addresses and whether it was replaced
// (agenthub.Hub).
type PanelHub interface {
	PanelInfo(ctx context.Context) (agentproto.PanelInfo, error)
	MarkSuperseded(ours, theirs int64, why ...any)
}

func NewFeeder(db *store.DB, ca *pki.CA, src *walship.Source, masterKey []byte, log *slog.Logger, now func() time.Time) *Feeder {
	if now == nil {
		now = time.Now
	}
	return &Feeder{db: db, ca: ca, src: src, masterKey: masterKey, log: log, now: now,
		limiter: auth.NewLimiter(10, 6*time.Second).WithClock(now),
		states:  map[int64]*State{}, cancel: map[int64]context.CancelFunc{}, kick: map[int64]chan struct{}{}}
}

// UseHub has the feeder tell standbys the panel's addresses and refuse
// those that know a newer panel. Call before serving.
func (f *Feeder) UseHub(h PanelHub) { f.hub = h }

// PushPanels tells every connected standby the panel's addresses again
// (they changed).
func (f *Feeder) PushPanels(context.Context) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range f.kick {
		select {
		case k <- struct{}{}:
		default:
		}
	}
}

// OnAgentURL registers what to do when a standby reports a new address of
// its agent port (agents should learn it). Call before serving.
func (f *Feeder) OnAgentURL(fn func(ctx context.Context)) { f.onAgentURL = fn }

// Register adds the feeder's endpoints to the agents' port.
func (f *Feeder) Register(mount func(pattern string, h http.HandlerFunc)) {
	mount("POST "+PathEnroll, f.handleEnroll)
	mount("POST "+PathRenew, f.handleRenew)
	mount("POST "+PathReplicate, f.handleReplicate)
}

// State returns what is known of standby id's copy.
func (f *Feeder) State(id int64) State {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s := f.states[id]; s != nil {
		return *s
	}
	return State{}
}

// Disconnect ends standby id's stream (it was removed).
func (f *Feeder) Disconnect(id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.cancel[id]; c != nil {
		c()
	}
	delete(f.states, id)
}

func (f *Feeder) handleEnroll(w http.ResponseWriter, r *http.Request) {
	ip := remoteIP(r)
	if !f.limiter.Allow(ip) {
		http.Error(w, "too many attempts", http.StatusTooManyRequests)
		return
	}
	var req EnrollRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxEnrollBody)).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	id, err := f.db.ConsumeStandbyToken(r.Context(), auth.HashToken(req.Token), f.now())
	if errors.Is(err, store.ErrNotFound) {
		f.log.Warn("standby enrollment rejected: invalid or expired token", "ip", ip)
		http.Error(w, "invalid or expired enrollment token", http.StatusForbidden)
		return
	}
	if err != nil {
		f.internalError(w, "consume standby token", err)
		return
	}
	der, err := f.issue(r.Context(), id, req.CSR)
	if err != nil {
		f.log.Warn("standby enrollment failed", "standby_id", id, "err", err)
		http.Error(w, "certificate request rejected: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := f.db.Audit(r.Context(), nil, "standby.enrolled", ip, map[string]any{"standby_id": id}); err != nil {
		f.log.Error("audit", "err", err)
	}
	f.log.Info("standby panel enrolled", "standby_id", id, "ip", ip)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(EnrollResponse{StandbyID: id, Cert: der, CACert: f.ca.Cert.Raw})
}

func (f *Feeder) issue(ctx context.Context, id int64, csr []byte) ([]byte, error) {
	now := f.now()
	der, err := f.ca.SignStandby(csr, id, now)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	if err := f.db.SetStandbyCert(ctx, id, pki.Fingerprint(der), cert.NotAfter, now); err != nil {
		return nil, err
	}
	return der, nil
}

// authorize returns the standby the request's certificate belongs to, or
// answers the request itself.
func (f *Feeder) authorize(w http.ResponseWriter, r *http.Request) (*store.Standby, bool) {
	// Before anything else: a former panel must not tell the standby it
	// was removed, nor give it a certificate.
	if f.superseded(w, r) {
		return nil, false
	}
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
		http.Error(w, "client certificate required", http.StatusUnauthorized)
		return nil, false
	}
	leaf := r.TLS.VerifiedChains[0][0]
	id, err := pki.StandbyID(leaf)
	if err != nil {
		http.Error(w, "not a standby panel's certificate", http.StatusForbidden)
		return nil, false
	}
	sb, err := f.db.StandbyByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "the standby panel was removed", http.StatusGone)
		return nil, false
	}
	if err != nil {
		f.internalError(w, "load standby", err)
		return nil, false
	}
	if fp := pki.Fingerprint(leaf.Raw); fp != sb.CertFP && fp != sb.PrevCertFP {
		f.log.Warn("standby presented a revoked certificate", "standby_id", id, "ip", remoteIP(r))
		http.Error(w, "certificate revoked", http.StatusForbidden)
		return nil, false
	}
	return sb, true
}

// superseded refuses a standby that knows a newer panel than this one; it
// writes the response itself. Otherwise it puts this panel's epoch in the
// response.
func (f *Feeder) superseded(w http.ResponseWriter, r *http.Request) bool {
	epoch, err := f.db.PanelEpoch(r.Context())
	if err != nil {
		f.internalError(w, "load panel epoch", err)
		return true
	}
	known, err := strconv.ParseInt(r.Header.Get(agentproto.HeaderPanelEpoch), 10, 64)
	if err == nil && known > epoch {
		if f.hub != nil {
			f.hub.MarkSuperseded(epoch, known, "standby_ip", remoteIP(r))
		}
		http.Error(w, "this panel was replaced by a standby that took over (epoch "+strconv.FormatInt(known, 10)+")", http.StatusConflict)
		return true
	}
	w.Header().Set(agentproto.HeaderPanelEpoch, strconv.FormatInt(epoch, 10))
	return false
}

func (f *Feeder) handleRenew(w http.ResponseWriter, r *http.Request) {
	sb, ok := f.authorize(w, r)
	if !ok {
		return
	}
	var req RenewRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxEnrollBody)).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	der, err := f.issue(r.Context(), sb.ID, req.CSR)
	if err != nil {
		http.Error(w, "certificate request rejected: "+err.Error(), http.StatusBadRequest)
		return
	}
	f.log.Info("standby certificate renewed", "standby_id", sb.ID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(RenewResponse{Cert: der})
}

// handleReplicate streams the master key and the database to a standby;
// the standby reports on its copy in the request body.
func (f *Feeder) handleReplicate(w http.ResponseWriter, r *http.Request) {
	sb, ok := f.authorize(w, r)
	if !ok {
		return
	}
	if r.ProtoMajor != 2 {
		http.Error(w, "HTTP/2 required", http.StatusHTTPVersionNotSupported)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	ip := remoteIP(r)
	f.mu.Lock()
	if old := f.cancel[sb.ID]; old != nil {
		old() // a reconnect replaces the stale stream
	}
	f.cancel[sb.ID] = cancel
	st := &State{Connected: true, Since: f.now()}
	f.states[sb.ID] = st
	// Standbys before 0.6 do not send their epoch nor know the addresses.
	var kick chan struct{}
	if f.hub != nil && r.Header.Get(agentproto.HeaderPanelEpoch) != "" {
		kick = make(chan struct{}, 1)
		kick <- struct{}{} // the first time, after the master key
		f.kick[sb.ID] = kick
	}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		if f.states[sb.ID] == st {
			st.Connected = false
			delete(f.cancel, sb.ID)
		}
		if kick != nil && f.kick[sb.ID] == kick {
			delete(f.kick, sb.ID)
		}
		f.mu.Unlock()
	}()

	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil {
		return
	}
	f.log.Info("standby panel connected", "standby_id", sb.ID, "ip", ip)
	f.db.SetStandbySeen(ctx, sb.ID, ip, f.now())

	// Reports, until the stream ends.
	go func() {
		defer cancel()
		sc := bufio.NewScanner(r.Body)
		sc.Buffer(make([]byte, 4096), 4096)
		last := f.now()
		agentURL := sb.AgentURL
		for sc.Scan() {
			var rep Report
			if json.Unmarshal(sc.Bytes(), &rep) != nil {
				return
			}
			if len(rep.Error) > 512 {
				rep.Error = rep.Error[:512]
			}
			if len(rep.Version) > 64 {
				rep.Version = rep.Version[:64]
			}
			if rep.AgentURL != "" && agentproto.ValidPanelURL(rep.AgentURL) != nil {
				rep.AgentURL = ""
			}
			f.mu.Lock()
			st.Report, st.ReportAt = rep, f.now()
			f.mu.Unlock()
			if rep.AgentURL != "" && rep.AgentURL != agentURL {
				agentURL = rep.AgentURL
				changed, err := f.db.SetStandbyAgentURL(ctx, sb.ID, agentURL)
				if err != nil {
					f.log.Error("record the standby's agent address", "err", err)
				} else if changed && f.onAgentURL != nil {
					go f.onAgentURL(context.WithoutCancel(ctx))
				}
			}
			if now := f.now(); now.Sub(last) >= seenEvery {
				last = now
				f.db.SetStandbySeen(ctx, sb.ID, ip, now)
			}
		}
	}()

	var wmu sync.Mutex
	send := func(m walship.Msg) error {
		wmu.Lock()
		defer wmu.Unlock()
		rc.SetWriteDeadline(time.Now().Add(writeTimeout))
		defer rc.SetWriteDeadline(time.Time{})
		if err := writeFrame(w, m); err != nil {
			return err
		}
		return rc.Flush()
	}
	if err := send(walship.Msg{Kind: kindMasterKey, Data: f.masterKey}); err != nil {
		return
	}
	go func() {
		t := time.NewTicker(pingEvery)
		defer t.Stop()
		for {
			msg := walship.Msg{Kind: kindPing}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			case <-kick: // nil without a hub: never
				info, err := f.hub.PanelInfo(ctx)
				if err != nil {
					f.log.Error("panel addresses", "err", err)
					continue
				}
				msg = walship.Msg{Kind: kindPanels}
				msg.Data, _ = json.Marshal(info)
			}
			if send(msg) != nil {
				cancel()
				return
			}
		}
	}()
	err := f.src.Follow(ctx, send)
	f.log.Info("standby panel disconnected", "standby_id", sb.ID, "reason", err)
}

func (f *Feeder) internalError(w http.ResponseWriter, what string, err error) {
	f.log.Error(what, "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

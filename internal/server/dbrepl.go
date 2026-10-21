package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/notify"
	"github.com/wiefix/basalt/internal/store"
)

// Replication of the MariaDB databases of sites served by several servers:
// each replica copies the databases of the sites it serves from the site's
// primary server, through the agents' tunnel (a loopback port on the
// replica forwards to the source's MariaDB socket). The panel works out
// who copies what, prepares the servers and the tunnels, tells each
// replica what to copy and records how it goes; the agents do the rest.

const (
	// replPortBase + a source's id is the loopback port its replicas reach
	// it at.
	replPortBase   = 30000
	maxReplSource  = 65535 - replPortBase
	dbReplTimeout  = 2 * time.Hour
	peerApplyEvery = 10 * time.Minute
	// Copying that has not worked for replAlertAfter is notified (a
	// reconnect or a seed under way is not).
	replAlertAfter = 5 * time.Minute
)

// errReplSourceOffline: the source cannot be reached through the panel
// (notified as the server being offline, not as replication).
const errReplSourceOffline = "the source server is offline"

func replPasswordAD(serverID int64) []byte {
	return []byte("mariadb-repl:" + strconv.FormatInt(serverID, 10))
}

// dbReplPlan is who copies which databases from whom.
type dbReplPlan struct {
	// sources: source server -> its databases replicas copy.
	sources map[int64][]*store.Database
	// copies: replica -> source -> databases.
	copies map[int64]map[int64][]*store.Database
}

func (s *Server) dbReplPlan(ctx context.Context) (*dbReplPlan, error) {
	p := &dbReplPlan{sources: map[int64][]*store.Database{}, copies: map[int64]map[int64][]*store.Database{}}
	sites, err := s.db.ReplicatedSites(ctx)
	if err != nil {
		return nil, err
	}
	for _, site := range sites {
		dbs, err := s.db.Databases(ctx, site.ID)
		if err != nil {
			return nil, err
		}
		if len(dbs) == 0 {
			continue
		}
		p.sources[site.ServerID] = append(p.sources[site.ServerID], dbs...)
		for _, r := range site.Replicas {
			if p.copies[r] == nil {
				p.copies[r] = map[int64][]*store.Database{}
			}
			p.copies[r][site.ServerID] = append(p.copies[r][site.ServerID], dbs...)
		}
	}
	return p, nil
}

// peerAddr is where other agents reach a server's tunnel.
func peerAddr(srv *store.Server) string {
	host := srv.LastIP
	if addrs := srv.DNSAddrs(); len(addrs) > 0 {
		host = addrs[0].String()
	}
	if host == "" || srv.PeerPort == 0 {
		return ""
	}
	return net.JoinHostPort(host, strconv.Itoa(srv.PeerPort))
}

// peerApply is a server's tunnel setup under plan: who may reach its
// MariaDB, and its forwards to the sources it copies from.
func (s *Server) peerApply(plan *dbReplPlan, serverID int64, servers map[int64]*store.Server) agentproto.PeerApply {
	req := agentproto.PeerApply{Revision: s.now().UnixNano(), Services: map[string][]int64{}, Forwards: []agentproto.PeerForward{}}
	var readers []int64
	for r, srcs := range plan.copies {
		if _, ok := srcs[serverID]; ok {
			readers = append(readers, r)
		}
	}
	slices.Sort(readers)
	if len(readers) > 0 {
		req.Services[agentproto.ServiceMariaDB] = readers
	}
	var srcs []int64
	for src := range plan.copies[serverID] {
		srcs = append(srcs, src)
	}
	slices.Sort(srcs)
	for _, src := range srcs {
		sv := servers[src]
		if sv == nil || src > maxReplSource {
			continue
		}
		if addr := peerAddr(sv); addr != "" {
			req.Forwards = append(req.Forwards, agentproto.PeerForward{Listen: "127.0.0.1:" + strconv.FormatInt(replPortBase+src, 10),
				ServerID: src, Addr: addr, Service: agentproto.ServiceMariaDB})
		}
	}
	return req
}

// peerState remembers the tunnel setup each server was last given (by its
// content) so that it is pushed when it changes or the agent reconnects.
type peerState struct {
	mu   sync.Mutex
	sent map[int64]string
	at   map[int64]time.Time
}

// PushPeers gives a server its tunnel setup (on connect, and when it
// changes).
func (s *Server) PushPeers(ctx context.Context, serverID int64, force bool) {
	plan, err := s.dbReplPlan(ctx)
	if err != nil {
		s.log.Error("plan database replication", "err", err)
		return
	}
	servers, err := s.serverMap(ctx)
	if err != nil {
		s.log.Error("list servers", "err", err)
		return
	}
	s.pushPeer(ctx, plan, servers, serverID, force)
}

func (s *Server) serverMap(ctx context.Context) (map[int64]*store.Server, error) {
	list, err := s.db.ListServers(ctx)
	if err != nil {
		return nil, err
	}
	out := map[int64]*store.Server{}
	for _, sv := range list {
		out[sv.ID] = sv
	}
	return out, nil
}

func (s *Server) pushPeer(ctx context.Context, plan *dbReplPlan, servers map[int64]*store.Server, serverID int64, force bool) {
	if !s.agents.Connected(serverID) {
		return
	}
	req := s.peerApply(plan, serverID, servers)
	b, _ := json.Marshal(struct {
		S map[string][]int64
		F []agentproto.PeerForward
	}{req.Services, req.Forwards})
	key := string(b)
	s.peers.mu.Lock()
	if s.peers.sent == nil {
		s.peers.sent, s.peers.at = map[int64]string{}, map[int64]time.Time{}
	}
	same := s.peers.sent[serverID] == key && time.Since(s.peers.at[serverID]) < peerApplyEvery
	s.peers.mu.Unlock()
	if same && !force {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := s.agents.Call(cctx, serverID, agentproto.TypePeerApply, req, nil); err != nil {
		s.log.Warn("tunnel setup", "server_id", serverID, "err", err)
		return
	}
	s.peers.mu.Lock()
	s.peers.sent[serverID], s.peers.at[serverID] = key, time.Now()
	s.peers.mu.Unlock()
}

// replPassword returns a source's replication password, made on first use.
func (s *Server) replPassword(ctx context.Context, serverID int64) (string, error) {
	sealed, err := s.db.ReplPassword(ctx, serverID)
	if err != nil {
		return "", err
	}
	if sealed == nil {
		if err := s.db.SetReplPassword(ctx, serverID, s.box.Seal([]byte(rand.Text()+rand.Text()), replPasswordAD(serverID))); err != nil {
			return "", err
		}
		if sealed, err = s.db.ReplPassword(ctx, serverID); err != nil {
			return "", err
		}
	}
	pw, err := s.box.Open(sealed, replPasswordAD(serverID))
	return string(pw), err
}

// syncDBReplication makes every replica copy what the plan says and
// records how it goes. A server offline is left for the next round.
func (s *Server) syncDBReplication(ctx context.Context) {
	if !s.dbReplRun.TryLock() {
		s.dbReplAgain.Store(true) // the round going on runs once more
		return
	}
	defer s.dbReplRun.Unlock()
	for {
		s.dbReplAgain.Store(false)
		s.dbReplRound(ctx)
		if !s.dbReplAgain.Load() || ctx.Err() != nil {
			return
		}
	}
}

func (s *Server) dbReplRound(ctx context.Context) {
	plan, err := s.dbReplPlan(ctx)
	if err != nil {
		s.log.Error("plan database replication", "err", err)
		return
	}
	servers, err := s.serverMap(ctx)
	if err != nil {
		s.log.Error("list servers", "err", err)
		return
	}
	known, err := s.db.DBReplications(ctx)
	if err != nil {
		s.log.Error("list database replication", "err", err)
		return
	}
	prev := map[[2]int64]store.DBReplication{}
	for _, k := range known {
		prev[[2]int64{k.ReplicaID, k.SourceID}] = k
	}
	record := func(r int64, st agentproto.DBReplState) {
		s.recordDBRepl(ctx, r, st, prev[[2]int64{r, st.ServerID}], plan.copies[r][st.ServerID], servers)
	}
	// Who takes part: sources, replicas, and replicas that copied before
	// (they drop what they no longer copy).
	involved := map[int64]bool{}
	for src := range plan.sources {
		involved[src] = true
	}
	for r := range plan.copies {
		involved[r] = true
	}
	for _, k := range known {
		involved[k.ReplicaID] = true
	}
	// 1. Every server taking part: its identity and binary log; a source
	// also its replication user.
	ready := map[int64]bool{}
	setupErr := map[int64]string{}
	for id := range involved {
		if !s.agents.Connected(id) {
			continue
		}
		req := agentproto.DBReplSetup{}
		if dbs, ok := plan.sources[id]; ok {
			pw, err := s.replPassword(ctx, id)
			if err != nil {
				s.log.Error("replication password", "server_id", id, "err", err)
				continue
			}
			req.Password = pw
			for _, d := range dbs {
				req.Databases = append(req.Databases, d.Name)
			}
		} else if len(plan.copies[id]) == 0 {
			ready[id] = true // only drops what it copied
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		var resp agentproto.DBReplSetupResponse
		err := s.agents.Call(cctx, id, agentproto.TypeDBReplSetup, req, &resp)
		cancel()
		if err != nil {
			setupErr[id] = err.Error()
			s.log.Warn("database replication setup", "server_id", id, "err", err)
			continue
		}
		if resp.Restarted {
			s.log.Warn("MariaDB restarted to turn its binary log on", "server_id", id)
		}
		ready[id] = true
	}
	// 2. Tunnels.
	for id := range involved {
		s.pushPeer(ctx, plan, servers, id, false)
	}
	// 3. Each replica: what to copy from whom.
	for r := range involved {
		srcs := plan.copies[r]
		if !s.agents.Connected(r) || !ready[r] {
			continue
		}
		req := agentproto.DBReplApply{Sources: []agentproto.DBReplSource{}}
		var keep []int64
		var problems []agentproto.DBReplState
		for src, dbs := range srcs {
			keep = append(keep, src)
			st := agentproto.DBReplState{ServerID: src, Lag: -1}
			switch {
			case setupErr[src] != "":
				st.Error = "the source: " + setupErr[src]
			case setupErr[r] != "":
				st.Error = setupErr[r]
			case !s.agents.Connected(src) || !ready[src]:
				st.Error = errReplSourceOffline
			case src > maxReplSource || peerAddr(servers[src]) == "":
				st.Error = "the source server's tunnel is off or has no address"
			}
			if st.Error == "" {
				source, err := s.replSource(ctx, src, dbs)
				if err == nil {
					req.Sources = append(req.Sources, source)
					continue
				}
				st.Error = err.Error()
			}
			// Its copies stay as they are until the source is back.
			req.Keep = append(req.Keep, src)
			problems = append(problems, st)
		}
		for _, p := range problems {
			record(r, p)
		}
		cctx, cancel := context.WithTimeout(ctx, dbReplTimeout)
		var resp agentproto.DBReplApplyResponse
		err := s.agents.Stream(cctx, r, agentproto.TypeDBReplApply, req, &resp, func(json.RawMessage) error { return nil })
		cancel()
		if err != nil {
			s.log.Warn("database replication", "server_id", r, "err", err)
			for _, src := range req.Sources {
				record(r, agentproto.DBReplState{ServerID: src.ServerID, Lag: -1, Error: err.Error()})
			}
			continue
		}
		for _, st := range resp.Sources {
			record(r, st)
		}
		if err := s.db.DeleteDBReplication(ctx, r, append(keep, req.Keep...)); err != nil {
			s.log.Error("forget database replication", "err", err)
		}
	}
}

// replSource is what a replica needs to copy dbs from src.
func (s *Server) replSource(ctx context.Context, src int64, dbs []*store.Database) (agentproto.DBReplSource, error) {
	pw, err := s.replPassword(ctx, src)
	if err != nil {
		return agentproto.DBReplSource{}, err
	}
	users := make([]string, len(dbs))
	for i, d := range dbs {
		users[i] = d.Username
	}
	cctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	var hashes agentproto.DBUserHashesResponse
	if err := s.agents.Call(cctx, src, agentproto.TypeDBUserHashes, agentproto.DBUserHashes{Users: users}, &hashes); err != nil {
		return agentproto.DBReplSource{}, fmt.Errorf("the source: %w", err)
	}
	out := agentproto.DBReplSource{ServerID: src, Port: int(replPortBase + src), Password: pw}
	for _, d := range dbs {
		h := hashes.Hashes[d.Username]
		if h == "" {
			return out, errors.New("the source has no user " + d.Username)
		}
		out.Databases = append(out.Databases, agentproto.DBCopy{Name: d.Name, User: d.Username, Hash: h})
	}
	return out, nil
}

// recordDBRepl records how replica copies from a source (prev: the last
// record) and notifies the sites whose databases dbs are when copying has
// stopped working for replAlertAfter, and when it works again.
func (s *Server) recordDBRepl(ctx context.Context, replica int64, st agentproto.DBReplState, prev store.DBReplication,
	dbs []*store.Database, servers map[int64]*store.Server) {
	r := store.DBReplication{ReplicaID: replica, SourceID: st.ServerID, Running: st.Running,
		Lag: st.Lag, Error: st.Error, Databases: st.Databases, UpdatedAt: s.now()}
	if r.Databases == nil {
		r.Databases = []string{}
	}
	var change replChange
	r.FailingSince, r.Alerted, change = replAlert(prev, st, r.UpdatedAt)
	if err := s.db.SetDBReplication(ctx, r); err != nil {
		s.log.Error("record database replication", "err", err)
		return
	}
	if change != replNoChange {
		s.notifyDBRepl(ctx, change, r, dbs, servers)
	}
}

type replChange int

const (
	replNoChange replChange = iota
	replStopped
	replResumed
)

// replAlert works out since when copying has failed and whether that was
// notified, from the last record and the new state at now.
func replAlert(prev store.DBReplication, st agentproto.DBReplState, now time.Time) (since time.Time, alerted bool, change replChange) {
	if st.Error == errReplSourceOffline {
		return prev.FailingSince, prev.Alerted, replNoChange
	}
	if st.Error == "" && st.Running {
		if prev.Alerted {
			return time.Time{}, false, replResumed
		}
		return time.Time{}, false, replNoChange
	}
	since = prev.FailingSince
	if since.IsZero() {
		since = now
	}
	if !prev.Alerted && now.Sub(since) >= replAlertAfter {
		return since, true, replStopped
	}
	return since, prev.Alerted, replNoChange
}

// notifyDBRepl tells each site whose databases dbs are that copying them
// to replica stopped or works again.
func (s *Server) notifyDBRepl(ctx context.Context, change replChange, r store.DBReplication, dbs []*store.Database, servers map[int64]*store.Server) {
	name := func(id int64) string {
		if sv := servers[id]; sv != nil {
			return sv.Name
		}
		return "server #" + strconv.FormatInt(id, 10)
	}
	var sites []int64
	for _, d := range dbs {
		if !slices.Contains(sites, d.SiteID) {
			sites = append(sites, d.SiteID)
		}
	}
	for _, id := range sites {
		site, err := s.db.SiteByID(ctx, id)
		if err != nil {
			continue
		}
		m := notify.Message{Event: EventReplication, At: r.UpdatedAt, Link: fmt.Sprintf("/sites/%d", site.ID), Project: site.ProjectID}
		if change == replStopped {
			m.Title = fmt.Sprintf("The databases of %s are not copied to %s", site.Name, name(r.ReplicaID))
			m.Text = fmt.Sprintf("Copying from %s has not worked since %s: %s. The copy of the site there keeps working with the databases of %s; they cannot take its place until copying works again.",
				name(r.SourceID), r.FailingSince.UTC().Format("2006-01-02 15:04 UTC"), replProblem(r), name(r.SourceID))
		} else {
			m.Title = fmt.Sprintf("The databases of %s are copied to %s again", site.Name, name(r.ReplicaID))
		}
		s.notify(m)
	}
}

// replProblem says what is wrong with copying r.
func replProblem(r store.DBReplication) string {
	if r.Error != "" {
		return r.Error
	}
	return "it is not running"
}

// startDBReplication runs a round in the background (after a change).
func (s *Server) startDBReplication() {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), dbReplTimeout)
		defer cancel()
		s.syncDBReplication(ctx)
	}()
}

// replicaDBProblem says why a site's databases cannot be copied from
// primary to replica ("" if they can).
func replicaDBProblem(primary, replica *store.Server) string {
	for _, sv := range []*store.Server{primary, replica} {
		switch {
		case sv.MariaDB == "":
			return "the site has databases and MariaDB is not installed on " + sv.Name
		case sv.PeerPort == 0:
			return "the site has databases, copied through the servers' tunnel, which is off on " + sv.Name +
				" (the agent's --peer-listen; the installer sets it)"
		}
	}
	if peerAddr(primary) == "" {
		return primary.Name + " has no address other servers reach it at"
	}
	if primary.ID > maxReplSource {
		return "server id too large for database replication"
	}
	return ""
}

// replicatedDBProblem says why a database of site could not be copied to
// the servers serving copies of the site ("" if it can, or if there are
// none).
func (s *Server) replicatedDBProblem(r *http.Request, site *store.Site) (string, error) {
	if len(site.Replicas) == 0 {
		return "", nil
	}
	primary, err := s.db.ServerByID(r.Context(), site.ServerID)
	if err != nil {
		return "", err
	}
	for _, id := range site.Replicas {
		replica, err := s.db.ServerByID(r.Context(), id)
		if err != nil {
			return "", err
		}
		if msg := replicaDBProblem(primary, replica); msg != "" {
			return msg, nil
		}
	}
	return "", nil
}

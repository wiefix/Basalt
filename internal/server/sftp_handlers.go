package server

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/auth"
	"github.com/wiefix/basalt/internal/sftpd"
	"github.com/wiefix/basalt/internal/store"
)

var sftpUsernameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{2,31}$`)

const maxSFTPKeys = 20

type sftpUserView struct {
	ID          int64     `json:"id"`
	Username    string    `json:"username"`
	HasPassword bool      `json:"has_password"`
	PublicKeys  []string  `json:"public_keys"` // fingerprints, never the raw keys
	CreatedAt   time.Time `json:"created_at"`
}

func viewSFTPUser(u *store.SFTPUser) sftpUserView {
	v := sftpUserView{ID: u.ID, Username: u.Username, HasPassword: u.PasswordHash != "", CreatedAt: u.CreatedAt, PublicKeys: []string{}}
	for _, k := range u.PublicKeys {
		if pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(k)); err == nil {
			v.PublicKeys = append(v.PublicKeys, pk.Type()+" "+ssh.FingerprintSHA256(pk))
		}
	}
	return v
}

// PushSFTP sends all SFTP accounts of a server to its agent.
func (s *Server) PushSFTP(ctx context.Context, serverID int64) error {
	if !s.agents.Connected(serverID) {
		return nil
	}
	rev, users, err := s.db.DesiredSFTP(ctx, serverID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return s.agents.Call(ctx, serverID, agentproto.TypeSFTPApply, agentproto.SFTPApply{Revision: rev, Users: users}, nil)
}

// sftpConnectionView is where SFTP clients connect.
type sftpConnectionView struct {
	Port               int    `json:"port"`
	HostKeyFingerprint string `json:"host_key_fingerprint"`
}

// sftpConnection is nil while the server has not reported its SFTP server.
func (s *Server) sftpConnection(ctx context.Context, serverID int64) *sftpConnectionView {
	port, fp, err := s.db.ServerSFTP(ctx, serverID)
	if err != nil || port == 0 {
		return nil
	}
	return &sftpConnectionView{Port: port, HostKeyFingerprint: fp}
}

func (s *Server) handleListSFTPUsers(w http.ResponseWriter, r *http.Request, p *principal) {
	site, ok := s.pathSite(w, r)
	if !ok {
		return
	}
	users, err := s.db.SFTPUsers(r.Context(), site.ID)
	if err != nil {
		s.internalError(w, r, "list sftp users", err)
		return
	}
	out := make([]sftpUserView, len(users))
	for i, u := range users {
		out[i] = viewSFTPUser(u)
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out, "connection": s.sftpConnection(r.Context(), site.ServerID)})
}

type createSFTPUserRequest struct {
	Username string `json:"username"`
	// Password: empty with no keys means "generate one"; "-" disables
	// password login (keys only).
	Password   string   `json:"password"`
	PublicKeys []string `json:"public_keys"`
}

func (s *Server) handleCreateSFTPUser(w http.ResponseWriter, r *http.Request, p *principal) {
	site, ok := s.pathSite(w, r)
	if !ok {
		return
	}
	var req createSFTPUserRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	username := strings.ToLower(strings.TrimSpace(req.Username))
	if !sftpUsernameRE.MatchString(username) {
		writeError(w, http.StatusUnprocessableEntity, "username must be 3-32 characters: a-z, 0-9, _ and -, starting with a letter")
		return
	}
	keys, ok := parseSFTPKeys(w, req.PublicKeys)
	if !ok {
		return
	}

	if req.Password == "-" && len(keys) == 0 {
		writeError(w, http.StatusUnprocessableEntity, "a key-only account needs at least one public key")
		return
	}
	hash, generated, ok := s.sftpPassword(w, r, req.Password)
	if !ok {
		return
	}

	u, err := s.db.CreateSFTPUser(r.Context(), site.ID, username, hash, keys)
	if errors.Is(err, store.ErrUsernameTaken) {
		writeError(w, http.StatusConflict, "this username is already in use")
		return
	}
	if err != nil {
		s.internalError(w, r, "create sftp user", err)
		return
	}
	s.audit(r, &p.user.ID, "sftp_user.created", map[string]any{"site_id": site.ID, "username": username, "keys": len(keys)})
	resp := map[string]any{"user": viewSFTPUser(u), "connection": s.sftpConnection(r.Context(), site.ServerID)}
	if generated != "" {
		resp["password"] = generated // shown once, never stored in clear
	}
	if err := s.PushSFTP(r.Context(), site.ServerID); err != nil {
		resp["sync_error"] = err.Error()
	}
	writeJSON(w, http.StatusCreated, resp)
}

// parseSFTPKeys checks authorized_keys lines and normalizes them; it
// writes the error response itself.
func parseSFTPKeys(w http.ResponseWriter, in []string) ([]string, bool) {
	if len(in) > maxSFTPKeys {
		writeError(w, http.StatusUnprocessableEntity, "too many public keys")
		return nil, false
	}
	keys := []string{}
	for i, k := range in {
		pk, comment, _, rest, err := ssh.ParseAuthorizedKey([]byte(k))
		if err != nil || len(strings.TrimSpace(string(rest))) > 0 {
			writeError(w, http.StatusUnprocessableEntity, "public key "+strconv.Itoa(i+1)+" is not a valid authorized_keys line")
			return nil, false
		}
		line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pk)))
		if comment != "" {
			line += " " + comment
		}
		keys = append(keys, line)
	}
	return keys, true
}

// sftpPassword turns a requested password into its hash: "" generates one
// (returned to show once), "-" means none (keys only).
func (s *Server) sftpPassword(w http.ResponseWriter, r *http.Request, password string) (hash, generated string, ok bool) {
	switch password {
	case "-":
		return "", "", true
	case "":
		generated = rand.Text() // 26 chars, 130 bits
		password = generated
	default:
		if err := auth.ValidatePassword(password); err != nil {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return "", "", false
		}
	}
	hash, err := s.hasher.Hash(r.Context(), password)
	if err != nil {
		s.internalError(w, r, "hash password", err)
		return "", "", false
	}
	return hash, generated, true
}

type updateSFTPUserRequest struct {
	// Password, when present: a new password, "" to generate one, "-" to
	// turn password login off.
	Password *string `json:"password"`
	// PublicKeys, when present, replace the account's keys.
	PublicKeys *[]string `json:"public_keys"`
}

func (s *Server) handleUpdateSFTPUser(w http.ResponseWriter, r *http.Request, p *principal) {
	site, ok := s.pathSite(w, r)
	if !ok {
		return
	}
	uid, err := strconv.ParseInt(r.PathValue("uid"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "sftp user not found")
		return
	}
	var req updateSFTPUserRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Password == nil && req.PublicKeys == nil {
		writeError(w, http.StatusUnprocessableEntity, "nothing to change")
		return
	}
	var keys *[]string
	if req.PublicKeys != nil {
		k, ok := parseSFTPKeys(w, *req.PublicKeys)
		if !ok {
			return
		}
		keys = &k
	}
	var hash *string
	generated := ""
	if req.Password != nil {
		h, g, ok := s.sftpPassword(w, r, *req.Password)
		if !ok {
			return
		}
		hash, generated = &h, g
	}
	u, err := s.db.UpdateSFTPUser(r.Context(), site.ID, uid, hash, keys)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "sftp user not found")
		return
	case errors.Is(err, store.ErrNoSFTPLogin):
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	case err != nil:
		s.internalError(w, r, "update sftp user", err)
		return
	}
	details := map[string]any{"site_id": site.ID, "username": u.Username}
	if hash != nil {
		details["password"] = map[bool]string{true: "removed", false: "changed"}[*hash == ""]
	}
	if keys != nil {
		details["keys"] = len(*keys)
	}
	s.audit(r, &p.user.ID, "sftp_user.updated", details)
	resp := map[string]any{"user": viewSFTPUser(u)}
	if generated != "" {
		resp["password"] = generated
	}
	if err := s.PushSFTP(r.Context(), site.ServerID); err != nil {
		resp["sync_error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleDeleteSFTPUser(w http.ResponseWriter, r *http.Request, p *principal) {
	site, ok := s.pathSite(w, r)
	if !ok {
		return
	}
	uid, err := strconv.ParseInt(r.PathValue("uid"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "sftp user not found")
		return
	}
	if err := s.db.DeleteSFTPUser(r.Context(), site.ID, uid); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "sftp user not found")
		return
	} else if err != nil {
		s.internalError(w, r, "delete sftp user", err)
		return
	}
	s.audit(r, &p.user.ID, "sftp_user.deleted", map[string]any{"site_id": site.ID, "sftp_user_id": uid})
	resp := map[string]any{"ok": true}
	if err := s.PushSFTP(r.Context(), site.ServerID); err != nil {
		resp["sync_error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleSFTPBans lists the addresses a server's SFTP server refuses after
// failed logins.
func (s *Server) handleSFTPBans(w http.ResponseWriter, r *http.Request, p *principal) {
	s.sftpBans(w, r, p, "")
}

func (s *Server) handleSFTPUnban(w http.ResponseWriter, r *http.Request, p *principal) {
	ip, err := netip.ParseAddr(r.PathValue("ip"))
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid IP address")
		return
	}
	s.sftpBans(w, r, p, ip.String())
}

func (s *Server) sftpBans(w http.ResponseWriter, r *http.Request, p *principal, unban string) {
	srv, ok := s.pathServer(w, r)
	if !ok {
		return
	}
	if !s.agents.Connected(srv.ID) {
		writeError(w, http.StatusConflict, "the server is offline")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	var resp agentproto.SFTPBansResponse
	if err := s.agents.Call(ctx, srv.ID, agentproto.TypeSFTPBans, agentproto.SFTPBans{Unban: unban}, &resp); err != nil {
		writeAgentError(w, err)
		return
	}
	if unban != "" {
		if !resp.Unbanned {
			writeError(w, http.StatusNotFound, "this address is not banned")
			return
		}
		s.audit(r, &p.user.ID, "sftp.unbanned", map[string]any{"server_id": srv.ID, "ip": unban})
	}
	if resp.Bans == nil {
		resp.Bans = []sftpd.Ban{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"bans": resp.Bans})
}

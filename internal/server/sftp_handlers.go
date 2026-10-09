package server

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/auth"
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

func (s *Server) sftpConnection(ctx context.Context, serverID int64) map[string]any {
	port, fp, err := s.db.ServerSFTP(ctx, serverID)
	if err != nil || port == 0 {
		return nil
	}
	return map[string]any{"port": port, "host_key_fingerprint": fp}
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
	if len(req.PublicKeys) > maxSFTPKeys {
		writeError(w, http.StatusUnprocessableEntity, "too many public keys")
		return
	}
	var keys []string
	for i, k := range req.PublicKeys {
		pk, comment, _, rest, err := ssh.ParseAuthorizedKey([]byte(k))
		if err != nil || len(strings.TrimSpace(string(rest))) > 0 {
			writeError(w, http.StatusUnprocessableEntity, "public key "+strconv.Itoa(i+1)+" is not a valid authorized_keys line")
			return
		}
		line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pk)))
		if comment != "" {
			line += " " + comment
		}
		keys = append(keys, line)
	}

	password := req.Password
	generated := ""
	switch {
	case password == "-":
		if len(keys) == 0 {
			writeError(w, http.StatusUnprocessableEntity, "a key-only account needs at least one public key")
			return
		}
		password = ""
	case password == "":
		generated = rand.Text() // 26 chars, 130 bits
		password = generated
	default:
		if err := auth.ValidatePassword(password); err != nil {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
	}
	hash := ""
	if password != "" {
		var err error
		if hash, err = s.hasher.Hash(r.Context(), password); err != nil {
			s.internalError(w, r, "hash password", err)
			return
		}
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

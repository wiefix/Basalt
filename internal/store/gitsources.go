package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// GitSource is a GitHub App the panel created (manifest flow) to reach
// the user's repositories.
type GitSource struct {
	ID      int64
	Kind    string // "github"
	Name    string
	AppID   int64 // GitHub's id of the app
	Slug    string
	Owner   string // the account or organization it belongs to
	HTMLURL string
	// Secrets is the encrypted JSON of its credentials.
	Secrets   []byte
	CreatedAt time.Time
}

const gitSourceQuery = `SELECT id, kind, name, app_id, slug, owner, html_url, secrets, created_at FROM git_sources`

func scanGitSource(sc interface{ Scan(...any) error }) (*GitSource, error) {
	var g GitSource
	var created int64
	if err := sc.Scan(&g.ID, &g.Kind, &g.Name, &g.AppID, &g.Slug, &g.Owner, &g.HTMLURL, &g.Secrets, &created); err != nil {
		return nil, err
	}
	g.CreatedAt = time.Unix(created, 0)
	return &g, nil
}

// CreateGitSource records a git source; secrets is set by the caller
// (encrypted) once the id is known, with SetGitSourceSecrets.
func (db *DB) CreateGitSource(ctx context.Context, g *GitSource) error {
	var created int64
	err := db.W.QueryRowContext(ctx,
		`INSERT INTO git_sources (kind, name, app_id, slug, owner, html_url, secrets) VALUES (?, ?, ?, ?, ?, ?, ?)
		 RETURNING id, created_at`,
		g.Kind, g.Name, g.AppID, g.Slug, g.Owner, g.HTMLURL, g.Secrets).Scan(&g.ID, &created)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return ErrNameTaken
		}
		return err
	}
	g.CreatedAt = time.Unix(created, 0)
	return nil
}

// SetGitSourceSecrets replaces a source's encrypted credentials.
func (db *DB) SetGitSourceSecrets(ctx context.Context, id int64, secrets []byte) error {
	return db.execOne(ctx, `UPDATE git_sources SET secrets = ? WHERE id = ?`, secrets, id)
}

// GitSources lists the git sources.
func (db *DB) GitSources(ctx context.Context) ([]*GitSource, error) {
	rows, err := db.R.QueryContext(ctx, gitSourceQuery+` ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*GitSource{}
	for rows.Next() {
		g, err := scanGitSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// GitSourceByID returns a git source or ErrNotFound.
func (db *DB) GitSourceByID(ctx context.Context, id int64) (*GitSource, error) {
	g, err := scanGitSource(db.R.QueryRowContext(ctx, gitSourceQuery+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return g, err
}

// DeleteGitSource removes a git source; its apps keep their repository URL
// but can no longer clone a private one (they lose the source).
func (db *DB) DeleteGitSource(ctx context.Context, id int64) error {
	return db.execOne(ctx, `DELETE FROM git_sources WHERE id = ?`, id)
}

// AppsOfRepo lists the git apps built from repo ("owner/name") through
// source.
func (db *DB) AppsOfRepo(ctx context.Context, source int64, repo string) ([]*App, error) {
	rows, err := db.R.QueryContext(ctx, appQuery+` WHERE a.git_source_id = ? AND a.git_repo = ? COLLATE NOCASE ORDER BY a.id, d.domain`, source, repo)
	if err != nil {
		return nil, err
	}
	return scanApps(rows)
}

-- basalt:foreign-keys-off
-- Projects group sites, apps (with container databases), DNS zones and
-- health checks; users get a role in each project they belong to. Every
-- existing resource goes to the project "Default" (id 1), which cannot be
-- deleted. (Foreign keys are off only so that columns with a REFERENCES
-- clause and a default can be added; SQLite refuses that with them on.)
CREATE TABLE projects (
    id          INTEGER PRIMARY KEY,
    name        TEXT    NOT NULL UNIQUE COLLATE NOCASE,
    description TEXT    NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL DEFAULT (unixepoch())
) STRICT;

INSERT INTO projects (id, name, description) VALUES (1, 'Default', '');

CREATE TABLE project_members (
    project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role       TEXT    NOT NULL CHECK (role IN ('admin', 'member', 'viewer')),
    created_at INTEGER NOT NULL DEFAULT (unixepoch()),
    PRIMARY KEY (project_id, user_id)
) STRICT;

CREATE INDEX project_members_user ON project_members(user_id);

ALTER TABLE sites ADD COLUMN project_id INTEGER NOT NULL DEFAULT 1 REFERENCES projects(id);
ALTER TABLE apps ADD COLUMN project_id INTEGER NOT NULL DEFAULT 1 REFERENCES projects(id);
ALTER TABLE dns_zones ADD COLUMN project_id INTEGER NOT NULL DEFAULT 1 REFERENCES projects(id);
ALTER TABLE health_checks ADD COLUMN project_id INTEGER NOT NULL DEFAULT 1 REFERENCES projects(id);

CREATE INDEX sites_project ON sites(project_id);
CREATE INDEX apps_project ON apps(project_id);
CREATE INDEX dns_zones_project ON dns_zones(project_id);
CREATE INDEX health_checks_project ON health_checks(project_id);

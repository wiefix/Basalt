-- An API token can be limited to one project: it then reaches only that
-- project's resources (and goes with the project). Audit entries about a
-- project's resources carry its ID in their details; this index finds them.
ALTER TABLE api_tokens ADD COLUMN project_id INTEGER REFERENCES projects(id) ON DELETE CASCADE;

CREATE INDEX audit_log_project ON audit_log (json_extract(details, '$.project_id'));

package server

import (
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/catalog"
	"github.com/wiefix/basalt/internal/dbengine"
	"github.com/wiefix/basalt/internal/dns"
	"github.com/wiefix/basalt/internal/files"
	"github.com/wiefix/basalt/internal/mailserver"
	"github.com/wiefix/basalt/internal/php"
	"github.com/wiefix/basalt/internal/sftpd"
	"github.com/wiefix/basalt/internal/store"
)

// Responses several routes share.
var (
	siteResp     = obj("site", siteView{}, "sync", proxySyncView{})
	appResp      = obj("app", appView{})
	appSyncResp  = obj("app", appView{}, "sync", proxySyncView{})
	okResp       = obj("ok", true)
	policyResp   = obj("policy", policyView{})
	tokensResp   = obj("tokens", []tokenView{})
	userResp     = obj("user", userView{})
	backupResp   = obj("backup", backupView{})
	panelRunResp = obj("backup", panelBackupView{})
	checkResp    = obj("check", checkView{})
	storageResp  = obj("storage", storageView{})
	zoneResp     = obj("zone", dnsZoneView{})
	recordResp   = obj("record", dnsRecordView{})
	channelResp  = obj("channel", channelView{})
	webhookResp  = obj("webhook", webhookView{})
	previewsResp = obj("domain", "", "previews", []appView{}, "max", 0, "supported", false)
	trashResp    = obj("entries", []agentproto.TrashEntry{}, "keep_days", 0)
	bansResp     = obj("bans", []sftpd.Ban{})
	accessResp   = obj("entries", []agentproto.AccessEntry{})
	sourceResp   = obj("source", (*catalogSourceView)(nil))
	deployResp   = obj("deployment", deploymentView{})
	updatesResp  = obj("enabled", false, "current", "", "latest", "", "available", false, "checked_at", time.Time{},
		"error", "", "servers", []updateServerView{}, "running", false, "run", (*updateRun)(nil))
	mailSettingsResp = obj("relay", (*mailRelayView)(nil), "send_limit", 0)
	linksResp        = obj("app", appView{}, "attach_error?", "")
)

// apiDocs describes every API route (the access a route needs comes from
// its registration). TestOpenAPICoversRoutes keeps it complete; responses
// are checked against it in the tests (Options.CheckResponses).
var apiDocs = map[string]apiDoc{
	"GET /api/v1/openapi.json": {summary: "This API description (OpenAPI 3.1)", resp: map[string]any{}},
	"GET /api/v1/version":      {summary: "Panel version", resp: obj("version", "", "commit", "")},

	// Setup and signing in.
	"GET /api/v1/setup":       {summary: "Whether the first owner still has to be created", resp: obj("required", false)},
	"POST /api/v1/setup":      {summary: "Create the first owner with the setup token", body: setupRequest{}, resp: userResp},
	"POST /api/v1/auth/login": {summary: "Sign in with email and password (sets the session cookie)", body: loginRequest{}, resp: obj("mfa_required", false, "user?", userView{})},
	"POST /api/v1/auth/passkey/options": {summary: "Start signing in with a passkey (navigator.credentials.get options)",
		resp: obj("challenge_id", "", "options", map[string]any{})},
	"POST /api/v1/auth/passkey":      {summary: "Sign in with a passkey (sets the session cookie)", body: passkeyLoginRequest{}, resp: userResp},
	"POST /api/v1/auth/mfa":          {summary: "Complete a sign-in with a TOTP or recovery code", body: codeRequest{}, resp: obj("user", userView{}, "recovery_codes_remaining?", 0)},
	"POST /api/v1/auth/logout":       {summary: "Sign out this session", resp: noContent},
	"GET /api/v1/auth/me":            {summary: "The current user", resp: userResp},
	"POST /api/v1/auth/password":     {summary: "Change your password (signs out other sessions)", body: changePasswordRequest{}, resp: noContent},
	"POST /api/v1/auth/totp/setup":   {summary: "Start two-factor setup: a new TOTP secret", body: passwordRequest{}, resp: obj("secret", "", "uri", "")},
	"POST /api/v1/auth/totp/enable":  {summary: "Turn two-factor on with a code; returns recovery codes", body: codeRequest{}, resp: obj("recovery_codes", []string{})},
	"POST /api/v1/auth/totp/disable": {summary: "Turn two-factor off", body: disableTOTPRequest{}, resp: noContent},

	// Users, sessions, API tokens.
	"GET /api/v1/users":                     {summary: "List users", resp: obj("users", []userView{})},
	"POST /api/v1/users":                    {summary: "Create a user (up to your own role)", body: createUserRequest{}, resp: userResp},
	"PUT /api/v1/users/{id}":                {summary: "Rename a user, change their role or disable them", body: updateUserRequest{}, resp: userResp},
	"DELETE /api/v1/users/{id}":             {summary: "Delete a user with their sessions and API tokens", resp: noContent},
	"PUT /api/v1/users/{id}/password":       {summary: "Set another user's password and sign them out", body: resetPasswordRequest{}, resp: noContent},
	"GET /api/v1/account/sessions":          {summary: "Your signed-in sessions", resp: obj("sessions", []sessionView{})},
	"DELETE /api/v1/account/sessions":       {summary: "Sign out all your other sessions", resp: noContent},
	"DELETE /api/v1/account/sessions/{id}":  {summary: "Sign out one of your other sessions", resp: noContent},
	"GET /api/v1/account/passkeys":          {summary: "Your passkeys", resp: obj("passkeys", []passkeyView{})},
	"POST /api/v1/account/passkeys/options": {summary: "Start adding a passkey (navigator.credentials.create options)", body: passwordRequest{}, resp: obj("options", map[string]any{})},
	"POST /api/v1/account/passkeys":         {summary: "Add the passkey the browser created", body: addPasskeyRequest{}, resp: obj("passkey", passkeyView{})},
	"PUT /api/v1/account/passkeys/{id}":     {summary: "Rename one of your passkeys", body: renamePasskeyRequest{}, resp: noContent},
	"DELETE /api/v1/account/passkeys/{id}":  {summary: "Remove one of your passkeys", resp: noContent},
	"GET /api/v1/account/tokens":            {summary: "Your API tokens", resp: tokensResp},
	"POST /api/v1/account/tokens":           {summary: "Create an API token (the secret is returned once)", body: createTokenRequest{}, resp: obj("token", tokenView{}, "secret", "")},
	"DELETE /api/v1/account/tokens/{id}":    {summary: "Delete one of your API tokens", resp: noContent},
	"GET /api/v1/tokens":                    {summary: "Every user's API tokens", resp: tokensResp},
	"DELETE /api/v1/tokens/{id}":            {summary: "Revoke a user's API token", resp: noContent},
	"GET /api/v1/audit": {summary: "The audit log, newest first", query: []queryParam{
		q("before", "only entries with a smaller ID (paging)"), q("limit", "1 to 200, default 50"),
		q("actor", `a user ID, or "system" for the panel's own actions`), q("action", "an action prefix, e.g. app.")},
		resp: obj("entries", []auditView{}, "actions", []string{}, "more", false)},
	"GET /api/v1/audit/verify": {summary: "Verify the audit log's hash chain, and check it against the newest entry each server's agent was given", resp: auditCheckView{}},
	"POST /api/v1/audit/anchors/reset": {summary: "Accept the audit log as it is: every agent takes its newest entry even if older than the one it holds (after restoring the panel from a backup); owners, with a session",
		resp: obj("anchors", []auditAnchorView{})},
	"GET /api/v1/audit/export": {summary: "Download the audit log, oldest first, with each entry's signature", content: "download", query: []queryParam{
		q("format", "jsonl (default) or csv"), q("actor", `a user ID, or "system"`), q("action", "an action prefix, e.g. app.")}},
	"GET /api/v1/audit/retention": {summary: "How long audit entries are kept, and how many were removed", resp: auditRetentionView{}},
	"PUT /api/v1/audit/retention": {summary: "Set how long audit entries are kept (0: forever, or 90 to 3650 days); older ones are removed right away; owners, with a session",
		body: auditRetentionRequest{}, resp: auditRetentionView{}},

	// Projects.
	"GET /api/v1/projects":      {summary: "Projects (the default one first)", resp: obj("projects", []projectView{})},
	"POST /api/v1/projects":     {summary: "Create a project", body: projectRequest{}, resp: obj("project", projectView{})},
	"GET /api/v1/projects/{id}": {summary: "A project with its members and what it holds", resp: obj("project", projectView{}, "members", []projectMemberView{}, "usage", projectUsageView{})},
	"PUT /api/v1/projects/{id}/limits": {summary: "Set what a project may hold (0: no limit); counts are hard, the disk limit only notifies",
		body: store.ProjectLimits{}, resp: obj("project", projectView{}, "usage", projectUsageView{})},
	"PUT /api/v1/projects/{id}":    {summary: "Rename a project or change its description", body: projectRequest{}, resp: obj("project", projectView{})},
	"DELETE /api/v1/projects/{id}": {summary: "Delete an empty project (not the default one)", resp: noContent},
	"GET /api/v1/projects/{id}/audit": {summary: "The audit log of a project's resources, newest first (its admins)", query: []queryParam{
		q("before", "only entries with a smaller ID (paging)"), q("limit", "1 to 200, default 50"),
		q("actor", `a user ID, or "system"`), q("action", "an action prefix, e.g. app.")},
		resp: obj("entries", []auditView{}, "actions", []string{}, "more", false)},
	"GET /api/v1/projects/{id}/audit/export": {summary: "Download the audit log of a project's resources, oldest first (its admins)", content: "download", query: []queryParam{
		q("format", "jsonl (default) or csv"), q("actor", `a user ID, or "system"`), q("action", "an action prefix, e.g. app.")}},
	"POST /api/v1/projects/{id}/members":         {summary: "Add a user, by email, to a project (or change their role)", body: addMemberRequest{}, resp: obj("member", projectMemberView{})},
	"PUT /api/v1/projects/{id}/members/{uid}":    {summary: "Add a user to a project or change their role there", body: projectMemberRequest{}, resp: noContent},
	"DELETE /api/v1/projects/{id}/members/{uid}": {summary: "Remove a user from a project", resp: noContent},
	"PUT /api/v1/sites/{id}/project":             {summary: "Move a site to another project", body: moveRequest{}, resp: noContent},
	"PUT /api/v1/apps/{id}/project": {summary: "Move an app or database (with its placements' health checks) to another project",
		body: moveRequest{}, resp: noContent},
	"PUT /api/v1/dns/zones/{id}/project":     {summary: "Move a DNS zone to another project", body: moveRequest{}, resp: noContent},
	"PUT /api/v1/health-checks/{id}/project": {summary: "Move a health check to another project", body: moveRequest{}, resp: noContent},

	// Servers and proxy routes.
	"GET /api/v1/servers":                   {summary: "List servers", resp: obj("servers", []serverView{})},
	"POST /api/v1/servers":                  {summary: "Add a server; returns its one-time install command", body: createServerRequest{}, resp: obj("server", serverView{}, "enrollment", enrollmentView{})},
	"GET /api/v1/servers/{id}":              {summary: "A server with its latest metrics", resp: obj("server", serverView{}, "metrics?", metricsView{})},
	"DELETE /api/v1/servers/{id}":           {summary: "Remove a server (its agent loses access)", resp: noContent},
	"GET /api/v1/servers/{id}/metrics":      {summary: "A server's metrics", query: []queryParam{q("minutes", "how far back, default 60, at most 43200 (30 days); samples older than 6 hours are 5-minute averages")}, resp: obj("metrics", []metricsView{})},
	"GET /api/v1/servers/{id}/certificates": {summary: "Public certificates of the server's domains and their expiry", resp: obj("certificates", []certView{})},
	"POST /api/v1/servers/{id}/ping":        {summary: "Round trip to the server's agent", resp: obj("rtt_ms", 0.0, "agent_time", time.Time{})},
	"PUT /api/v1/servers/{id}/public-ips":   {summary: "Set the addresses automatic DNS records use (empty: detected)", body: publicIPsRequest{}, resp: obj("server", serverView{})},
	"GET /api/v1/panel":                     {summary: "This panel's epoch, and whether a standby panel took over from it", resp: obj("panel", panelView{})},
	"GET /api/v1/standbys":                  {summary: "Standby panels: copies of this panel that can take over", resp: obj("standbys", []standbyView{}, "available", false, "failover", failoverView{})},
	"POST /api/v1/standbys": {summary: "Add a standby panel; returns its one-time install command (owner)", body: createStandbyRequest{},
		resp: obj("standby", standbyView{}, "server", serverView{}, "enrollment", standbyEnrollmentView{})},
	"DELETE /api/v1/standbys/{id}": {summary: "Remove a standby panel (it stops getting the database; owner)", resp: noContent},
	"PUT /api/v1/standbys/failover": {summary: "Choose the standby panel that takes over by itself when the panel is gone, and after how long (owner)",
		body: failoverRequest{}, resp: obj("failover", failoverView{})},
	"POST /api/v1/servers/{id}/enroll-token":   {summary: "New install command for a server not enrolled yet", resp: obj("enrollment", enrollmentView{})},
	"GET /api/v1/servers/{id}/routes":          {summary: "A server's proxy routes and whether they are applied", resp: obj("routes", []routeView{}, "sync", proxySyncView{})},
	"POST /api/v1/servers/{id}/routes":         {summary: "Add a reverse proxy route", body: createRouteRequest{}, resp: obj("route", routeView{}, "sync", proxySyncView{})},
	"DELETE /api/v1/servers/{id}/routes/{rid}": {summary: "Remove a proxy route", resp: obj("sync", proxySyncView{})},

	// Sites, files, SFTP, MariaDB.
	"GET /api/v1/sites":      {summary: "List sites", resp: obj("sites", []siteView{})},
	"POST /api/v1/sites":     {summary: "Create a static or PHP site", body: createSiteRequest{}, resp: obj("site", siteView{}, "sync", proxySyncView{}, "provision_error?", "")},
	"GET /api/v1/sites/{id}": {summary: "A site and the servers serving it", resp: obj("site", siteView{}, "sync", proxySyncView{}, "placements", []sitePlacementView{})},
	"POST /api/v1/sites/{id}/replicas": {summary: "Serve a site from another server too (its files are copied there every minute)", body: replicaRequest{},
		resp: obj("site", siteView{}, "placements", []sitePlacementView{})},
	"DELETE /api/v1/sites/{id}/replicas/{sid}": {summary: "Stop serving a site from a server (its copy goes to that server's trash)",
		resp: obj("site", siteView{}, "placements", []sitePlacementView{}, "cleanup_error?", "")},
	"PUT /api/v1/sites/{id}/primary": {summary: "Make a server serving a copy of a site its primary (with force while the primary is offline)",
		body: sitePrimaryRequest{}, resp: obj("site", siteView{}, "placements", []sitePlacementView{}, "warning?", "")},
	"POST /api/v1/sites/{id}/sync": {summary: "Copy a site's files to the other servers now (in the background)",
		resp: obj("site", siteView{}, "placements", []sitePlacementView{})},
	"PUT /api/v1/sites/{id}/domains":      {summary: "Set a site's domains", body: siteDomainsRequest{}, resp: siteResp},
	"PUT /api/v1/sites/{id}/php":          {summary: "Switch a site between static and a PHP version", body: sitePHPRequest{}, resp: siteResp},
	"PUT /api/v1/sites/{id}/php-settings": {summary: "Change a site's PHP options (memory, upload size, time limits, errors, timezone)", body: php.Settings{}, resp: obj("site", siteView{})},
	"PUT /api/v1/sites/{id}/laravel":      {summary: "Turn a Laravel site's scheduler and queue workers on or off (systemd services of the site's user on its primary server)", body: php.Laravel{}, resp: obj("site", siteView{})},
	"GET /api/v1/sites/{id}/php-log":      {summary: "The end of a site's PHP error log", resp: obj("log", "", "size", 0, "truncated?", false, "path", "")},
	"GET /api/v1/sites/{id}/access-log":   {summary: "The latest requests to a site (newest first)", query: []queryParam{q("limit", "how many, 1–1000, default 200")}, resp: accessResp},
	"POST /api/v1/apps/{id}/password": {summary: "Give a container database a new password; linked apps holding its URL get the new one", body: dbPasswordRequest{},
		resp: obj("connection", connectionView{}, "updated_apps", []string{}, "redeploying", []string{}, "deploy_error?", "")},
	"GET /api/v1/apps/{id}/access-log":        {summary: "The latest requests to an app through the proxy (its main server)", query: []queryParam{q("limit", "how many, 1–1000, default 200")}, resp: accessResp},
	"GET /api/v1/sites/{id}/laravel":          {summary: "The state of a Laravel site's scheduler and queue workers (none when they are off)", resp: obj("processes", []php.LaravelProcess{})},
	"GET /api/v1/sites/{id}/laravel/log":      {summary: "The last lines a Laravel site's scheduler and queue workers wrote (?lines=, 1–1000, default 200), oldest first", resp: obj("lines", []php.LaravelLogLine{})},
	"POST /api/v1/sites/{id}/laravel/restart": {summary: "Restart a Laravel site's scheduler and queue workers (after new code is deployed); workers finish their job first", resp: obj("processes", []php.LaravelProcess{})},
	"POST /api/v1/sites/{id}/install": {summary: "Install WordPress, Laravel or Symfony into a PHP site's empty public folder, with a new database", body: siteInstallRequest{},
		resp: obj("app", "", "version", "", "files", 0, "database", databaseView{}, "url?", "", "admin?", wordpressAdmin{})},
	"GET /api/v1/sites/{id}/adminer":    {summary: "Whether Adminer is installed in a site, and its address", resp: obj("adminer", adminerView{})},
	"POST /api/v1/sites/{id}/adminer":   {summary: "Install Adminer in a PHP site (again: at a new address), for the server's MariaDB only", resp: obj("adminer", adminerView{})},
	"DELETE /api/v1/sites/{id}/adminer": {summary: "Remove Adminer from a site", resp: obj("adminer", adminerView{})},
	"POST /api/v1/sites/{id}/usage":     {summary: "Measure a site's disk space (files and databases) now", resp: obj("site", siteView{})},
	"PUT /api/v1/sites/{id}/quota":      {summary: "Set a site's soft disk quota (0: none); going over it is notified", body: siteQuotaRequest{}, resp: obj("site", siteView{})},
	"DELETE /api/v1/sites/{id}":         {summary: "Delete a site (its files go to the server's trash)", resp: obj("sync", proxySyncView{}, "cleanup_error?", "")},
	"GET /api/v1/sites/{id}/files":      {summary: "List a directory", query: []queryParam{q("path", "directory in the site, e.g. /public")}, resp: obj("path", "", "entries", []files.Entry{})},
	"GET /api/v1/sites/{id}/files/stat": {summary: "Details of a file or directory",
		query: []queryParam{q("path", "path in the site")}, resp: obj("entry", (*files.Entry)(nil))},
	"GET /api/v1/sites/{id}/files/text": {summary: "Read a text file (up to 1 MiB)", query: []queryParam{q("path", "path in the site")}, resp: obj("content", "", "size", 0)},
	"GET /api/v1/sites/{id}/files/content": {summary: "Download a file", content: "download",
		query: []queryParam{q("path", "path in the site")}},
	"PUT /api/v1/sites/{id}/files/content": {summary: "Upload a file (the raw body; up to 4 GiB)", content: "upload",
		query: []queryParam{q("path", "path in the site")}, resp: obj("path", "", "size", 0)},
	"POST /api/v1/sites/{id}/files/{op}": {summary: "File operation: mkdir, write, rename, chmod, delete, compress, extract",
		body: fileOpRequest{}, resp: obj("ok", true, "extract?", files.ExtractResult{})},
	"GET /api/v1/sites/{id}/sftp-users": {summary: "A site's SFTP accounts and connection details",
		resp: obj("users", []sftpUserView{}, "connection", (*sftpConnectionView)(nil))},
	"POST /api/v1/sites/{id}/sftp-users": {summary: "Create an SFTP account", body: createSFTPUserRequest{},
		resp: obj("user", sftpUserView{}, "connection", (*sftpConnectionView)(nil), "password?", "", "sync_error?", "")},
	"GET /api/v1/servers/{id}/trash":           {summary: "Files of deleted sites waiting on the server (removed after 30 days)", resp: trashResp},
	"DELETE /api/v1/servers/{id}/trash/{name}": {summary: "Remove a deleted site's files now", resp: trashResp},
	"POST /api/v1/servers/{id}/trash/{name}/restore": {summary: "Make a deleted site's files a new site on that server (its server_id is the path's)",
		body: createSiteRequest{}, resp: obj("site", siteView{}, "provision_error?", "")},
	"GET /api/v1/servers/{id}/sftp-bans":         {summary: "Addresses the server's SFTP refuses after failed logins", resp: bansResp},
	"DELETE /api/v1/servers/{id}/sftp-bans/{ip}": {summary: "Let a banned address use SFTP again", resp: bansResp},
	"PUT /api/v1/sites/{id}/sftp-users/{uid}": {summary: "Change an SFTP account's password or public keys", body: updateSFTPUserRequest{},
		resp: obj("user", sftpUserView{}, "password?", "", "sync_error?", "")},
	"DELETE /api/v1/sites/{id}/sftp-users/{uid}": {summary: "Delete an SFTP account", resp: obj("ok", true, "sync_error?", "")},
	"GET /api/v1/sites/{id}/databases":           {summary: "A site's MariaDB databases", resp: obj("databases", []databaseView{}, "mariadb", "")},
	"POST /api/v1/sites/{id}/databases": {summary: "Create a MariaDB database and user (the password is returned once)", body: createDatabaseRequest{},
		resp: obj("database", databaseView{}, "credentials", dbCredentialsView{})},
	"POST /api/v1/sites/{id}/databases/{did}/password": {summary: "New password for a database's user", resp: obj("credentials", dbCredentialsView{})},
	"DELETE /api/v1/sites/{id}/databases/{did}":        {summary: "Drop a database and its user", resp: noContent},

	// Backups of sites and apps ({target} is /api/v1/sites/{id} or /api/v1/apps/{id}).
	"GET {target}/backups": {summary: "Backup settings, history and what a backup holds",
		resp: obj("policy", (*policyView)(nil), "backups", []backupView{}, "restic", "", "items", []agentproto.BackupItem{})},
	"POST {target}/backups":             {summary: "Start a backup", resp: backupResp},
	"GET {target}/backups/{bid}":        {summary: "A backup or restore run with its log", resp: backupResp},
	"GET {target}/backups/{bid}/stream": {summary: "Follow a run's log", content: "stream"},
	"PUT {target}/backup-policy":        {summary: "Set where and when to back up", body: policyRequest{}, resp: policyResp},
	"DELETE {target}/backup-policy":     {summary: "Stop backing up", resp: okResp},
	"GET {target}/snapshots":            {summary: "Snapshots in the storage", resp: obj("snapshots", []agentproto.SnapshotInfo{})},
	"POST {target}/restore":             {summary: "Restore snapshots", body: restoreRequest{}, resp: backupResp},
	"GET {target}/snapshots/{sid}/files": {summary: "List a folder of a backup of a site's files or an app's volume",
		query: []queryParam{q("path", "folder, e.g. public; empty for the top"), q("volume", "apps: the volume")},
		resp:  obj("path", "", "entries", []agentproto.SnapshotEntry{})},
	"GET {target}/snapshots/{sid}/download": {summary: "Download a file of a backup, a folder as a zip archive, or a database dump",
		content: "download", query: []queryParam{q("path", "file or folder; empty for all of it"), q("volume", "apps: the volume"),
			q("database", "a database whose dump to download instead")}},
	"POST {target}/snapshots/{sid}/restore-copy": {summary: "Restore a file or folder of a backup into a new folder of the site or volume", body: restoreCopyRequest{},
		resp: agentproto.RestoreCopyResponse{}},
	"GET /api/v1/updates":                       {summary: "Basalt versions: this panel, each server, the newest release, the last update", resp: updatesResp},
	"POST /api/v1/updates/check":                {summary: "Look for a newer release now", resp: updatesResp},
	"POST /api/v1/updates/apply":                {summary: "Update every online server to the newest release (the panel's host restarts the panel)", resp: obj("run", updateRun{})},
	"GET /api/v1/backup-storages":               {summary: "List backup storages", resp: obj("storages", []storageView{})},
	"POST /api/v1/backup-storages":              {summary: "Add a backup storage (S3, SFTP or local)", body: storageRequest{}, resp: storageResp},
	"PUT /api/v1/backup-storages/{id}":          {summary: "Change a backup storage", body: storageRequest{}, resp: storageResp},
	"DELETE /api/v1/backup-storages/{id}":       {summary: "Remove a backup storage (its backups stay)", resp: okResp},
	"GET /api/v1/backup-storages/{id}/password": {summary: "The storage's repository password", resp: obj("password", "")},
	"POST /api/v1/backup-storages/{id}/check":   {summary: "Open (or create) the repository from a server", body: checkStorageRequest{}, resp: obj("ok", true, "error?", "")},
	"GET /api/v1/backup-checks":                 {summary: "Repositories holding backups with their latest integrity check", resp: obj("repositories", []checkRepoView{})},
	"POST /api/v1/backup-checks":                {summary: "Check a repository's integrity now (restic check)", body: startCheckRequest{}, resp: checkResp},
	"GET /api/v1/backup-checks/{cid}":           {summary: "An integrity check with its log", resp: checkResp},
	"GET /api/v1/panel-backup": {summary: "Backups of the panel itself: recovery passphrase, policy, history",
		resp: obj("recovery", obj("set", false, "set_at?", time.Time{}), "policy", (*policyView)(nil), "backups", []panelBackupView{},
			"restic", "", "restic_error?", "")},
	"PUT /api/v1/panel-backup/recovery":   {summary: "Set the recovery passphrase that protects the master key in backups", body: recoveryRequest{}, resp: okResp},
	"PUT /api/v1/panel-backup/policy":     {summary: "Set where and when to back up the panel", body: policyRequest{}, resp: policyResp},
	"DELETE /api/v1/panel-backup/policy":  {summary: "Stop backing up the panel", resp: okResp},
	"POST /api/v1/panel-backup/run":       {summary: "Back up the panel now", resp: panelRunResp},
	"GET /api/v1/panel-backup/runs/{bid}": {summary: "A panel backup with its log", resp: panelRunResp},

	// DNS.
	"GET /api/v1/dns": {summary: "Zones, nameservers and the DNS servers' state",
		resp: obj("zones", []dnsZoneView{}, "nameservers", []string{}, "servers", []dnsServerView{})},
	"PUT /api/v1/dns/nameservers": {summary: "Set the nameservers new zones get", body: nameserversRequest{}, resp: obj("nameservers", []string{})},
	"POST /api/v1/dns/zones":      {summary: "Add a zone", body: createZoneRequest{}, resp: zoneResp},
	"GET /api/v1/dns/zones/{id}": {summary: "A zone with its records and automatic records",
		resp: obj("zone", dnsZoneView{}, "records", []dnsRecordView{}, "auto", []dns.AutoRecord{}, "types", []string{})},
	"DELETE /api/v1/dns/zones/{id}":               {summary: "Delete a zone", resp: noContent},
	"PUT /api/v1/dns/zones/{id}/dnssec":           {summary: "Sign a zone with DNSSEC or stop", body: dnssecRequest{}, resp: zoneResp},
	"POST /api/v1/dns/zones/{id}/records":         {summary: "Add a record", body: dns.Record{}, resp: recordResp},
	"PUT /api/v1/dns/zones/{id}/records/{rid}":    {summary: "Replace a record", body: dns.Record{}, resp: recordResp},
	"DELETE /api/v1/dns/zones/{id}/records/{rid}": {summary: "Delete a record", resp: noContent},

	// Monitoring and notifications.
	"GET /api/v1/health-checks":             {summary: "Health checks with their status and each server's result", resp: obj("checks", []healthCheckView{})},
	"POST /api/v1/health-checks":            {summary: "Add a health check", body: healthCheckRequest{}, resp: obj("check", obj("id", 0, "name", ""))},
	"PUT /api/v1/health-checks/{id}":        {summary: "Change a health check (its status starts over)", body: healthCheckRequest{}, resp: noContent},
	"DELETE /api/v1/health-checks/{id}":     {summary: "Delete a health check", resp: noContent},
	"GET /api/v1/health-checks/{id}/events": {summary: "A health check's status changes", resp: obj("events", []healthEventView{})},
	"GET /api/v1/notifications": {summary: "The notification channels you manage (all for panel admins, your projects' for project admins) and the events",
		resp: obj("channels", []channelView{}, "events", notificationEvents, "panel_url", "")},
	"POST /api/v1/notifications":           {summary: "Add a notification channel of the panel or of a project", body: channelRequest{}, resp: channelResp},
	"PUT /api/v1/notifications/{id}":       {summary: "Change a channel (empty secrets keep the current ones)", body: channelRequest{}, resp: channelResp},
	"DELETE /api/v1/notifications/{id}":    {summary: "Delete a channel", resp: noContent},
	"POST /api/v1/notifications/{id}/test": {summary: "Send a test message", resp: obj("ok", true, "error?", "")},
	"PUT /api/v1/notifications/panel-url":  {summary: "Set the panel address links in messages use", body: panelURLRequest{}, resp: obj("panel_url", "")},

	// Catalog.
	"GET /api/v1/catalog": {summary: "Catalog templates and the external catalog source",
		resp: obj("templates", []templateView{}, "source", (*catalogSourceView)(nil))},
	"POST /api/v1/catalog/{id}/install": {summary: "Install a template as an app", body: installRequest{},
		resp: obj("app", appView{}, "notes", "", "attach_error?", "", "deploy_error?", "", "deployment?", deploymentView{})},
	"PUT /api/v1/catalog/source":   {summary: "Set the external catalog (saved only if it verifies)", body: catalogSourceRequest{}, resp: sourceResp},
	"POST /api/v1/catalog/refresh": {summary: "Download the external catalog now", resp: sourceResp},
	"GET /api/v1/apps/{id}/template": {summary: "An installed app's template and available update",
		resp: obj("installed_revision", 0, "template", (*templateInfoView)(nil), "update_available", false,
			"update", (*templateUpdate)(nil), "backups", false, "images?", []imageChange{}, "needs?", []catalog.Input{})},
	"POST /api/v1/apps/{id}/template-update": {summary: "Update an app to its template's newest revision", body: templateUpdateRequest{},
		resp: obj("update", templateUpdate{})},

	// Mail.
	"GET /api/v1/mail": {summary: "Mail server, domains, deliverability checks and settings",
		resp: obj("server", (*appView)(nil), "domains", []mailDomainView{}, "ports", []int{}, "hostname?", "",
			"tls?", mailTLSStatus{}, "checks?", (*mailChecks)(nil), "settings?", mailSettingsResp)},
	"POST /api/v1/mail": {summary: "Set up the mail server", body: createMailRequest{},
		resp: obj("server", appView{}, "deploy_error?", "", "deployment?", deploymentView{})},
	"PUT /api/v1/mail/settings":                         {summary: "Outgoing relay and send limit", body: mailSettingsRequest{}, resp: obj("settings", mailSettingsResp, "warning?", "")},
	"POST /api/v1/mail/checks":                          {summary: "Run the deliverability checks now", resp: obj("checks", mailChecks{})},
	"POST /api/v1/mail/domains":                         {summary: "Add a mail domain", body: mailDomainRequest{}, resp: obj("domain", mailDomainView{})},
	"PUT /api/v1/mail/domains/{id}":                     {summary: "Set a domain's catch-all", body: mailDomainUpdate{}, resp: noContent},
	"DELETE /api/v1/mail/domains/{id}":                  {summary: "Remove a mail domain (without mailboxes or forwarders)", resp: noContent},
	"GET /api/v1/mail/domains/{id}/mailboxes":           {summary: "A domain's mailboxes", resp: obj("mailboxes", []mailserver.Mailbox{})},
	"POST /api/v1/mail/domains/{id}/mailboxes":          {summary: "Create a mailbox", body: mailboxRequest{}, resp: obj("mailbox", obj("id", "", "address", ""))},
	"PUT /api/v1/mail/domains/{id}/mailboxes/{mid}":     {summary: "Change a mailbox", body: mailboxRequest{}, resp: noContent},
	"DELETE /api/v1/mail/domains/{id}/mailboxes/{mid}":  {summary: "Delete a mailbox and its mail", resp: noContent},
	"GET /api/v1/mail/domains/{id}/forwarders":          {summary: "A domain's forwarding addresses", resp: obj("forwarders", []mailserver.Forwarder{})},
	"POST /api/v1/mail/domains/{id}/forwarders":         {summary: "Create a forwarding address", body: forwarderRequest{}, resp: obj("forwarder", mailserver.Forwarder{})},
	"PUT /api/v1/mail/domains/{id}/forwarders/{fid}":    {summary: "Change where an address forwards to", body: forwarderRequest{}, resp: noContent},
	"DELETE /api/v1/mail/domains/{id}/forwarders/{fid}": {summary: "Delete a forwarding address", resp: noContent},

	// Registries.
	"GET /api/v1/registries":         {summary: "Credentials for private image registries", resp: obj("registries", []registryView{})},
	"POST /api/v1/registries":        {summary: "Add registry credentials", body: registryRequest{}, resp: obj("registry", registryView{})},
	"PUT /api/v1/registries/{id}":    {summary: "Replace registry credentials", body: registryRequest{}, resp: obj("registry", registryView{})},
	"DELETE /api/v1/registries/{id}": {summary: "Remove registry credentials", resp: noContent},

	// Apps and container databases.
	"GET /api/v1/apps": {summary: "List apps, databases and the mail server", resp: obj("apps", []appView{})},
	"POST /api/v1/apps": {summary: "Create an app (image, git, compose) or a database", body: createAppRequest{},
		resp: obj("app", appView{}, "sync", proxySyncView{}, "warnings", []string{})},
	"GET /api/v1/apps/{id}":         {summary: "An app with its servers", resp: obj("app", appView{}, "sync", proxySyncView{}, "placements", []placementView{})},
	"PUT /api/v1/apps/{id}":         {summary: "Change an app's runtime settings", body: appSettingsRequest{}, resp: appResp},
	"DELETE /api/v1/apps/{id}":      {summary: "Delete an app", query: []queryParam{q("volumes", "1 to delete its volumes too")}, resp: obj("sync", proxySyncView{}, "cleanup_error?", "")},
	"PUT /api/v1/apps/{id}/domains": {summary: "Set an app's domains", body: appDomainsRequest{}, resp: appSyncResp},
	"PUT /api/v1/apps/{id}/routes": {summary: "Set the other services of a compose app that have domains of their own (a new service or port is served from the next deployment)",
		body: appRoutesRequest{}, resp: appSyncResp},
	"GET /api/v1/apps/{id}/env":                       {summary: "An app's environment variables", resp: obj("env", map[string]string{})},
	"PUT /api/v1/apps/{id}/env":                       {summary: "Replace an app's environment variables (applied on the next deployment)", body: appEnvRequest{}, resp: okResp},
	"POST /api/v1/apps/{id}/deploy":                   {summary: "Deploy an app, or roll back to an earlier deployment", body: deployRequest{}, resp: deployResp},
	"GET /api/v1/apps/{id}/deployments":               {summary: "An app's deployments", resp: obj("deployments", []deploymentView{})},
	"GET /api/v1/apps/{id}/deployments/{did}":         {summary: "A deployment with its log", resp: deployResp},
	"POST /api/v1/apps/{id}/deployments/{did}/cancel": {summary: "Stop a running deployment (the previous one keeps running)", resp: obj()},
	"GET /api/v1/apps/{id}/deployments/{did}/stream":  {summary: "Follow a deployment's log", content: "stream"},
	"GET /api/v1/apps/{id}/metrics":                   {summary: "An app's container metrics", query: []queryParam{q("minutes", "how far back, default 60, at most 43200 (30 days); samples older than 6 hours are 5-minute averages")}, resp: obj("metrics", []appMetricsView{})},
	"GET /api/v1/apps/{id}/status": {summary: "State of an app's containers", query: []queryParam{
		q("service", "compose service"), q("server", "server ID (apps on several servers)")}, resp: obj("status", agentproto.AppStatus{})},
	"GET /api/v1/apps/{id}/logs": {summary: "Last lines of an app's container output", query: []queryParam{
		q("tail", "number of lines"), q("service", "compose service"), q("server", "server ID")}, resp: obj("log", "")},
	"GET /api/v1/apps/{id}/logs/stream": {summary: "Follow an app's container output", content: "stream", query: []queryParam{
		q("tail", "lines to start with"), q("service", "compose service"), q("server", "server ID")}},
	"GET /api/v1/apps/{id}/compose": {summary: "A compose app's file (a git app's: the one last read from its repository), published service and warnings",
		resp: obj("compose", "", "service", "", "port", 0, "warnings", []string{}, "error?", "")},
	"PUT /api/v1/apps/{id}/compose": {summary: "Replace a compose app's file and published service (a git app's: the service only)", body: composeRequest{}, resp: obj("app", appView{}, "warnings", []string{})},
	"POST /api/v1/apps/{id}/compose/load": {summary: "Read a git app's compose file from its repository (head of its branch) and keep it",
		resp: obj("app", appView{}, "warnings", []string{}, "commit", "")},
	"POST /api/v1/apps/{id}/replicas":         {summary: "Run an app on another server too", body: replicaRequest{}, resp: obj("deploying", false, "app?", appView{}, "deploy_error?", "")},
	"DELETE /api/v1/apps/{id}/replicas/{sid}": {summary: "Stop running an app on a server", resp: obj("app?", appView{}, "cleanup_error?", "")},
	"POST /api/v1/apps/{id}/deploy-key":       {summary: "Replace a git app's deploy key", resp: appResp},
	"GET /api/v1/apps/{id}/webhook":           {summary: "A git app's push webhook", resp: webhookResp},
	"PUT /api/v1/apps/{id}/webhook":           {summary: "Turn deploy-on-push on or off, or make a new secret", body: webhookRequest{}, resp: webhookResp},
	"GET /api/v1/apps/{id}/previews":          {summary: "Where the app's pull (merge) requests are deployed, and their previews", resp: previewsResp},
	"PUT /api/v1/apps/{id}/previews": {summary: "Deploy each pull request into the app's branch at pr-<number>.<domain> (empty: no previews)",
		body: previewsRequest{}, resp: previewsResp},
	"POST /api/v1/webhooks/github": {summary: "Push webhook of a GitHub App the panel created: deploys the apps of the repository and branch (authenticated by its signature)",
		resp: obj("result", "", "deploying?", []string{})},
	"GET /api/v1/git-sources": {summary: "Git sources (GitHub Apps) apps can be built from; for those who can create apps",
		resp: obj("sources", []gitSourceView{}, "panel_url", "")},
	"POST /api/v1/git-sources": {summary: "Connect a GitLab or Gitea/Forgejo instance with an access token (checked first)",
		body: forgeSourceRequest{}, resp: obj("source", gitSourceView{})},
	"GET /api/v1/git-sources/{id}": {summary: "A git source and where its GitHub App is installed (asked of GitHub)",
		resp: obj("source", gitSourceView{}, "installations", []installationView{}, "error?", "")},
	"DELETE /api/v1/git-sources/{id}":    {summary: "Remove a git source (its apps keep their repository URL)", resp: noContent},
	"GET /api/v1/git-sources/{id}/repos": {summary: "Repositories the source's GitHub App is installed on", resp: obj("repos", []repoView{})},
	"GET /api/v1/git-sources/{id}/branches": {summary: "Branches of a repository of the source", query: []queryParam{q("repo", "owner/name")},
		resp: obj("branches", []string{})},
	"POST /api/v1/git-sources/github/manifest": {summary: "Start creating a GitHub App: the browser posts manifest to action",
		body: manifestRequest{}, resp: obj("action", "", "manifest", "", "state", "")},
	"POST /api/v1/git-sources/github/complete": {summary: "Finish creating a GitHub App with the code GitHub sent back",
		body: manifestCompleteRequest{}, resp: obj("source", gitSourceView{})},
	"POST /api/v1/webhooks/apps/{id}":  {summary: "Push (and, for GitLab and Gitea sources, merge or pull request) webhook from GitHub, GitLab or Gitea (authenticated by its signature)", resp: obj("result", "", "commit?", "")},
	"GET /api/v1/database-engines":     {summary: "Database engines and versions", resp: obj("engines", []dbengine.Engine{})},
	"GET /api/v1/apps/{id}/connection": {summary: "A database's connection details and credentials", resp: obj("connection", connectionView{})},
	"PUT /api/v1/apps/{id}/public-access": {summary: "Make a database reachable from outside its server on a port, from the listed addresses only (port 0: linked apps only)",
		body: publicAccessRequest{}, resp: obj("connection", connectionView{}, "apply_error?", "")},
	"GET /api/v1/apps/{id}/upgrade": {summary: "The major versions a database can be upgraded to, and an upgrade waiting for its backup", resp: dbUpgradeView{}},
	"POST /api/v1/apps/{id}/upgrade": {summary: "Upgrade a database to a newer major version: a deployment of it (PostgreSQL's data is dumped into a new cluster), after a backup if asked; no rollback goes back to the older version",
		body: dbUpgradeRequest{}, resp: dbUpgradeStarted{}},
	"POST /api/v1/apps/{id}/links":         {summary: "Let an app reach a database", body: linkRequest{}, resp: linksResp},
	"DELETE /api/v1/apps/{id}/links/{app}": {summary: "Stop letting an app reach a database", resp: linksResp},
}

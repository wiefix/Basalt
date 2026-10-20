// Typed client for the Basalt API. Types mirror the JSON views in
// internal/server/*_handlers.go.

export type Role = "owner" | "admin" | "member" | "viewer" | "user";

export interface User {
  id: number;
  email: string;
  name: string;
  role: Role;
  totp_enabled: boolean;
  disabled: boolean;
  passkeys?: number; // in the list of users
  created_at: string;
}

export interface Passkey {
  id: number;
  name: string;
  host: string; // the panel address it works on
  synced: boolean; // backed up by its password manager
  created_at: string;
  last_used_at: string | null;
}

export interface Server {
  id: number;
  name: string;
  status: "pending" | "online" | "offline";
  hostname?: string;
  os?: string;
  arch?: string;
  kernel?: string;
  distro?: string;
  agent_version?: string;
  cpu_count?: number;
  mem_total?: number;
  last_seen?: string;
  last_ip?: string;
  cert_expires?: string;
  enroll_expires?: string;
  php_limits?: boolean;
  peer_port?: number; // the agent's tunnel to other servers (0: off)
  peer_error?: string;
  mariadb?: string;
  php_versions: string[];
  isolated: boolean;
  restic: string; // version, "" when backups cannot run there
  docker: string;
  // How Docker isolates containers (absent when unknown).
  docker_security?: { userns: boolean; rootless?: boolean; seccomp?: string; apparmor: boolean; selinux?: boolean; cgroup_v2: boolean };
  // Addresses for automatic DNS records: found on the host, set by an
  // admin (replacing them), and the ones in use.
  detected_ips: string[];
  public_ips: string[];
  dns_ips: string[];
  created_at: string;
}

export interface Metrics {
  at: string;
  cpu_pct: number;
  mem_used: number;
  mem_total: number;
  disk_used: number;
  disk_total: number;
  load1: number;
  load5: number;
  load15: number;
  uptime: number;
}

export interface Enrollment {
  token: string;
  expires_at: string;
  install_command: string;
  command: string;
}

export interface Sync {
  revision: number;
  applied_revision: number;
  in_sync: boolean;
  error?: string;
}

export interface Route {
  id: number;
  kind: "static" | "reverse_proxy";
  domains: string[];
  root?: string;
  upstream?: string;
  site_id?: number;
  app_id?: number;
  tls: "auto" | "internal";
  hsts: boolean;
  created_at: string;
}

export interface Site {
  id: number;
  server_id: number;
  project_id: number;
  name: string;
  kind: "static";
  domains: string[];
  tls: "auto" | "internal";
  php_version: string;
  php_settings: PHPSettings;
  usage: { disk_bytes: number; disk_files: number; db_bytes: number; at: string | null };
  quota_mb: number;
  over_quota: boolean;
  replicas: number[]; // other servers serving a copy of the files
  created_at: string;
}

// SitePlacement is a server serving a site; a replica's copy of the files
// is refreshed from the primary every minute.
export interface SitePlacement {
  server_id: number;
  primary: boolean;
  health_check?: number;
  status?: "unknown" | "up" | "down";
  synced_at?: string;
  sync_error?: string;
  syncing?: boolean;
  // How the copy's databases follow the primary's (sites with databases).
  databases?: { running: boolean; lag: number; error?: string; copied: string[]; waiting: string[]; updated_at: string };
}

export interface PHPSettings {
  memory_mb?: number;
  upload_mb?: number;
  max_children?: number;
  max_execution_time?: number;
  max_input_vars?: number;
  display_errors?: boolean;
  timezone?: string;
  cpu_percent?: number;
  ram_mb?: number;
}

export interface FileEntry {
  name: string;
  type: "file" | "dir" | "symlink" | "other";
  size: number;
  mode: string;
  mod_time: string;
  target?: string;
}

export interface ExtractResult {
  files: number;
  dirs: number;
  bytes: number;
  skipped?: string[];
}

export interface SFTPUser {
  id: number;
  username: string;
  has_password: boolean;
  public_keys: string[];
  created_at: string;
}

export interface SFTPConnection {
  port: number;
  host_key_fingerprint: string;
}

export interface Database {
  id: number;
  name: string;
  username: string;
  created_at: string;
}

export interface DatabaseCredentials {
  host: string;
  database: string;
  username: string;
  password: string;
}

export interface AppVolume {
  name: string;
  path: string;
}

export interface GitSettings {
  url: string;
  branch: string;
  dockerfile: string;
  context: string;
}

export interface ComposeService {
  name: string;
  image: string;
  ports?: number[];
  job?: boolean;
}

export interface ComposeSummary {
  services: ComposeService[];
  volumes: string[];
}

export interface App {
  id: number;
  server_id: number;
  project_id: number;
  name: string;
  source: "image" | "git" | "compose" | "database" | "mail";
  image: string;
  git?: GitSettings;
  deploy_key?: string;
  auto_deploy: boolean;
  // Compose apps: the published service (port is its port) and the file's contents without secrets.
  service?: string;
  compose?: ComposeSummary;
  // Databases: engine id, major version and the apps that reach it.
  engine?: string;
  version?: string;
  links?: number[];
  port: number;
  domains: string[];
  tls: "auto" | "internal";
  volumes: AppVolume[];
  memory_mb: number;
  cpus: number;
  read_only: boolean;
  template?: string;
  template_revision?: number;
  template_update?: string;
  // Other servers the app runs on (image and git apps).
  replicas: number[];
  created_at: string;
}

// A server an app runs on, with the health check its copy follows.
export interface AppPlacement {
  server_id: number;
  primary: boolean;
  health_check?: number;
  status?: "unknown" | "up" | "down";
}

export interface AppSettings {
  image: string;
  git?: Partial<GitSettings>;
  port: number;
  volumes: AppVolume[];
  memory_mb: number;
  cpus: number;
  read_only?: boolean;
}

export interface AppMetrics {
  at: string;
  cpu_pct: number; // percent of one CPU
  mem_used: number;
  mem_limit: number;
  net_rx: number; // bytes since the containers started
  net_tx: number;
}

export interface Deployment {
  id: number;
  image: string;
  digests?: Record<string, string>;
  commit?: string;
  commit_message?: string;
  status: "running" | "succeeded" | "failed";
  error?: string;
  image_digest?: string;
  created_at: string;
  finished_at?: string;
  log?: string;
}

export interface AppStatus {
  state: string;
  health?: string;
  deployment_id?: number;
  started_at?: string;
  restart_count?: number;
  exit_code?: number;
  oom_killed?: boolean;
  error?: string;
  services?: ServiceStatus[];
}

export interface ServiceStatus {
  name: string;
  image: string;
  state: string;
  health?: string;
  deployment_id?: number;
  started_at?: string;
  restart_count?: number;
  exit_code?: number;
  oom_killed?: boolean;
}

export interface ComposeFile {
  compose: string;
  service: string;
  port: number;
  warnings: string[];
  error?: string;
}

export interface Engine {
  id: string;
  name: string;
  versions: string[];
  port: number;
  env_var: string;
  sql: boolean;
}

export interface DatabaseConnection {
  host: string;
  port: number;
  url: string;
  env_var: string;
  credentials: { database?: string; user?: string; password: string; root_password?: string };
}

export interface BackupStorage {
  id: number;
  name: string;
  kind: "s3" | "sftp" | "local";
  endpoint?: string;
  bucket?: string;
  region?: string;
  access_key?: string;
  host?: string;
  port?: number;
  user?: string;
  path: string;
  ssh_public_key?: string;
  policies: number;
  created_at: string;
}

export type StorageInput = Omit<BackupStorage, "id" | "policies" | "created_at" | "ssh_public_key"> & { secret_key?: string };

export interface BackupPolicy {
  storage_id: number;
  schedule: "manual" | "hourly" | "daily";
  at_minute: number; // UTC
  keep_last: number;
  keep_daily: number;
  keep_weekly: number;
  keep_monthly: number;
  next_run?: string;
}

export interface BackupItem {
  kind: "site_files" | "mariadb" | "volume" | "db_dump";
  site_id?: number;
  app_id?: number;
  volume?: string;
  database?: string;
}

export interface BackupRun {
  id: number;
  kind: "backup" | "restore";
  trigger: "manual" | "schedule";
  status: "running" | "succeeded" | "failed";
  error?: string;
  storage_id?: number;
  snapshots: { item: BackupItem; id?: string; snapshot?: string; bytes?: number; data_added?: number }[];
  bytes: number;
  data_added: number;
  created_at: string;
  finished_at?: string;
  log?: string;
}

export interface PanelBackupRun {
  id: number;
  trigger: "manual" | "schedule";
  status: "running" | "succeeded" | "failed";
  error?: string;
  snapshot?: string;
  bytes: number;
  created_at: string;
  finished_at?: string;
  log?: string;
}

// An integrity check (restic check) of a repository.
export interface BackupCheck {
  id: number;
  storage_id: number;
  server_id?: number; // absent: the panel's repository
  trigger: "manual" | "schedule";
  status: "running" | "succeeded" | "failed";
  read_data?: string; // the part of the data read back, "n/52"
  error?: string;
  created_at: string;
  finished_at?: string;
  log?: string;
}

// A repository holding backups: a server's in a storage, or the panel's (server_id 0).
export interface CheckRepository {
  storage_id: number;
  server_id: number;
  server: string;
  last: BackupCheck | null;
  next_at?: string;
}

export interface Snapshot {
  id: string;
  time: string;
  item: BackupItem;
  bytes: number;
}

export interface UpdatesInfo {
  enabled: boolean;
  current: string;
  latest: string;
  available: boolean;
  checked_at?: string;
  error: string;
  running: boolean;
  servers: { id: number; name: string; online: boolean; version: string; arch: string; updatable: boolean }[];
  run: {
    version: string;
    state: "downloading" | "preparing" | "restarting" | "done" | "failed";
    error?: string;
    started_at: string;
    finished_at?: string;
    by: string;
    servers: { id: number; name: string; status: string; error?: string }[];
  } | null;
}

export interface AccessEntry {
  at: string;
  ip: string;
  method: string;
  host: string;
  uri: string;
  proto: string;
  status: number;
  bytes: number;
  duration_ms: number;
  user_agent?: string;
}

export interface TrashEntry {
  name: string;
  site_id: number;
  deleted_at: string;
  bytes: number;
}

export interface SFTPBan {
  ip: string;
  until: string;
  strikes: number;
}

export interface SnapshotEntry {
  name: string;
  type: "file" | "dir" | "symlink" | "other";
  size: number;
  mode: string;
  mod_time: string;
}

export interface RestoredCopy {
  folder: string;
  files: number;
  dirs: number;
  bytes: number;
  skipped: number;
}

export interface Webhook {
  path: string;
  secret: string;
  auto_deploy: boolean;
}

export interface Registry {
  id: number;
  host: string;
  username: string;
  created_at: string;
  updated_at: string;
}

export interface DNSZone {
  id: number;
  project_id: number;
  name: string;
  serial: number;
  records: number;
  // Present when the zone is signed: what to give the registrar.
  dnssec?: { key_tag: number; algorithm: number; ds: string[]; dnskey: string };
  created_at: string;
  updated_at: string;
}

export interface DNSRecord {
  id: number;
  name: string;
  type: string;
  ttl: number;
  value: string;
  priority?: number;
  weight?: number;
  port?: number;
  flags?: number;
  tag?: string;
  // A/AAAA: served only while this health check is not down.
  health_check?: number;
}

export type DNSRecordInput = Omit<DNSRecord, "id">;

// An automatic record: A/AAAA for a site's or app's domain.
export type DNSAutoRecord = Omit<DNSRecord, "id"> & { source: string };

export interface DNSServer {
  server_id: number;
  name: string;
  online: boolean;
  port: number;
  listen_error?: string;
  ip?: string;
  in_sync: boolean;
  error?: string;
}

export interface MailDomain {
  id: number;
  name: string;
  dns_records: DNSRecordInput[]; // absolute names
  managed_dns: boolean;
  catch_all: string;
  created_at: string;
}

export interface TemplateInput {
  key: string;
  label: string;
  type: "domain" | "text" | "email";
  required: boolean;
  default?: string;
  help?: string;
}

export interface Template {
  id: string;
  name: string;
  description: string;
  category: string;
  website: string;
  version: string;
  revision: number;
  memory_mb: number;
  inputs: TemplateInput[];
  secrets: { key: string; length: number }[];
  expose: { service: string; port: number };
  notes: string;
  services: { name: string; image: string }[];
  volumes: string[];
  compose: string;
  source?: string;
}

export interface CatalogSource {
  url: string;
  key: string;
  allow_private: boolean;
  generated?: string;
  fetched_at?: string;
  error?: string;
  error_at?: string;
  templates: number;
  skipped: string[];
}

export interface AppTemplateInfo {
  installed_revision: number;
  template: { id: string; name: string; version: string; revision: number; notes: string; website: string } | null;
  update_available: boolean;
  images?: { service: string; from?: string; to?: string }[];
  needs?: TemplateInput[];
  backups: boolean;
  update: { revision: number; state: "backing_up" | "failed"; backup_id?: number; error?: string } | null;
}

export interface HealthCheck {
  id: number;
  project_id: number;
  name: string;
  kind: "http" | "https" | "tcp";
  host: string;
  port: number;
  path: string;
  host_header: string;
  insecure: boolean;
  interval: number;
  timeout: number;
  status: "unknown" | "up" | "down";
  status_since: string | null;
  results: { server_id: number; server_name: string; ok: boolean; latency_ms: number; error?: string; at: string; stale: boolean }[];
}

export interface HealthCheckInput {
  project_id?: number; // a new check's project
  name: string;
  kind: string;
  host: string;
  port: number;
  path: string;
  host_header: string;
  insecure: boolean;
  interval: number;
  timeout: number;
}

export type ChannelKind = "webhook" | "slack" | "discord" | "telegram" | "email";

export interface NotificationChannel {
  id: number;
  project_id: number; // 0: a channel of the whole panel
  name: string;
  kind: ChannelKind;
  events: string[];
  url?: string;
  secret?: string;
  allow_private?: boolean;
  chat_id?: string;
  smtp_host?: string;
  smtp_port?: number;
  smtp_tls?: "implicit" | "starttls" | "none";
  smtp_user?: string;
  smtp_insecure?: boolean;
  from?: string;
  to?: string[];
  host?: string; // where a secret URL points
  last_sent_at: string | null;
  last_error?: string;
  last_error_at?: string;
  queued: number; // messages waiting for another attempt
}

// NotificationEvent: project means a project's channels can get it.
export interface NotificationEvent {
  id: string;
  label: string;
  project: boolean;
}

// ChannelInput: secret fields left empty keep the current value.
export type ChannelInput = {
  project_id?: number; // a new channel's project (0: the whole panel)
  name: string;
  kind: ChannelKind;
  events: string[];
  url?: string;
  secret?: string;
  allow_private?: boolean;
  token?: string;
  chat_id?: string;
  smtp_host?: string;
  smtp_port?: number;
  smtp_tls?: string;
  smtp_user?: string;
  smtp_password?: string;
  smtp_insecure?: boolean;
  from?: string;
  to?: string[];
};

export interface Project {
  id: number;
  name: string;
  description: string;
  resources: number; // sites, apps, DNS zones and health checks it holds
  members: number;
  default: boolean;
  role: Role; // what you may do in it (a role on the panel counts everywhere)
  limits: ProjectLimits;
  created_at: string;
}

// ProjectLimits: what a project may hold, 0 is no limit. Counts are hard;
// the disk limit (files and databases of its sites) only notifies.
export interface ProjectLimits {
  sites: number;
  apps: number;
  databases: number;
  dns_zones: number;
  health_checks: number;
  disk_mb: number;
}

export interface ProjectUsage {
  sites: number;
  apps: number;
  databases: number;
  dns_zones: number;
  health_checks: number;
  disk_bytes: number; // the sites' last measurement
}

export type ProjectRole = "admin" | "member" | "viewer";

export interface ProjectMember {
  user_id: number;
  email: string;
  name: string;
  role: ProjectRole;
  created_at: string;
}

export type ProjectResource = "sites" | "apps" | "dns/zones" | "health-checks";

export interface SessionInfo {
  id: string;
  ip: string;
  user_agent: string;
  current: boolean;
  created_at: string;
  last_seen_at: string;
  expires_at: string;
}

export interface APIToken {
  id: number;
  name: string;
  prefix: string;
  role: Role;
  project_id?: number; // limited to this project
  user?: { id: number; email: string }; // in the list of all tokens
  expires_at: string | null;
  expired: boolean;
  last_used_at: string | null;
  last_used_ip?: string;
  created_at: string;
}

export interface OpenAPIOperation {
  summary: string;
  description: string;
  operationId: string;
  tags: string[];
  "x-basalt-role"?: Role;
  "x-basalt-project"?: boolean;
  parameters?: { name: string; in: "path" | "query"; description?: string }[];
  requestBody?: { content: Record<string, { schema: unknown }> };
  responses?: Record<string, { description: string; content?: Record<string, { schema: unknown }> }>;
}

export interface OpenAPISpec {
  info: { title: string; version: string };
  paths: Record<string, Record<string, OpenAPIOperation>>;
  components: { schemas: Record<string, unknown> };
}

export interface AuditEntry {
  id: number;
  at: string;
  actor: { id: number; name?: string; email?: string } | null; // null: the panel itself
  action: string;
  ip?: string;
  details: Record<string, unknown>;
}

export interface AuditCheck {
  entries: number;
  legacy: number; // written before the chain existed: not protected
  intact: boolean;
  broken_at?: number;
  at: string;
}

export interface MailRelay {
  host: string;
  port: number;
  tls: "implicit" | "starttls";
  username: string;
  has_password: boolean;
  insecure: boolean;
}

export interface MailRelayInput {
  host: string;
  port: number;
  tls: "implicit" | "starttls";
  username: string;
  password?: string;
  insecure: boolean;
}

export interface MailSettings {
  relay: MailRelay | null;
  send_limit: number;
}

export interface Forwarder {
  id: string;
  address: string;
  recipients: string[];
}

export interface Mailbox {
  id: string;
  address: string;
  name: string;
  aliases: string[];
  quota_mb: number;
  used_mb: number;
}

export type CheckStatus = "ok" | "warn" | "fail" | "unknown";

export interface MailCheckResult {
  domain?: string;
  check: "outbound_smtp" | "hostname" | "ptr" | "blocklist" | "mx" | "spf" | "dkim" | "dmarc";
  target?: string;
  status: CheckStatus;
  detail: string;
}

export interface MailChecks {
  at: string;
  results: MailCheckResult[];
  error?: string;
}

export interface MailOverview {
  server: App | null;
  hostname?: string;
  domains: MailDomain[];
  ports: number[];
  tls?: { not_after?: string; error?: string };
  checks?: MailChecks | null;
  settings?: MailSettings;
}

export interface MailboxInput {
  local?: string;
  name?: string;
  password?: string;
  quota_mb?: number;
  aliases?: string[];
}

export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
  ) {
    super(message);
  }
}

// Listeners notified when any request returns 401 (session expired).
const unauthorizedListeners = new Set<() => void>();
export function onUnauthorized(f: () => void) {
  unauthorizedListeners.add(f);
  return () => {
    unauthorizedListeners.delete(f);
  };
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const init: RequestInit = { method, credentials: "same-origin", headers: {} };
  if (body !== undefined) {
    init.body = JSON.stringify(body);
    (init.headers as Record<string, string>)["Content-Type"] = "application/json";
  }
  const res = await fetch(path, init);
  if (res.status === 204) return undefined as T;
  let data: any = null;
  const text = await res.text();
  if (text) {
    try {
      data = JSON.parse(text);
    } catch {
      data = { error: text };
    }
  }
  if (!res.ok) {
    if (res.status === 401 && !path.startsWith("/api/v1/auth/")) {
      unauthorizedListeners.forEach((f) => f());
    }
    throw new ApiError(res.status, data?.error ?? res.statusText);
  }
  return data as T;
}

const get = <T>(p: string) => request<T>("GET", p);
const post = <T>(p: string, b: unknown = {}) => request<T>("POST", p, b);
const put = <T>(p: string, b: unknown) => request<T>("PUT", p, b);
const del = <T>(p: string) => request<T>("DELETE", p);

const q = (path: string) => "?path=" + encodeURIComponent(path);

export const api = {
  setupStatus: () => get<{ required: boolean }>("/api/v1/setup"),
  setup: (b: { setup_token: string; email: string; name: string; password: string }) =>
    post<{ user: User }>("/api/v1/setup", b),

  login: (email: string, password: string) =>
    post<{ mfa_required: boolean; user?: User }>("/api/v1/auth/login", { email, password }),
  mfa: (code: string) =>
    post<{ user: User; recovery_codes_remaining?: number }>("/api/v1/auth/mfa", { code }),
  logout: () => post<void>("/api/v1/auth/logout"),
  me: () => get<{ user: User }>("/api/v1/auth/me"),
  changePassword: (current_password: string, new_password: string) =>
    post<void>("/api/v1/auth/password", { current_password, new_password }),
  totpSetup: (password: string) =>
    post<{ secret: string; uri: string }>("/api/v1/auth/totp/setup", { password }),
  totpEnable: (code: string) => post<{ recovery_codes: string[] }>("/api/v1/auth/totp/enable", { code }),
  totpDisable: (password: string, code: string) =>
    post<void>("/api/v1/auth/totp/disable", { password, code }),

  users: () => get<{ users: User[] }>("/api/v1/users"),
  updateUser: (id: number, b: { name?: string; role?: Role; disabled?: boolean }) =>
    put<{ user: User }>(`/api/v1/users/${id}`, b),
  deleteUser: (id: number) => del<void>(`/api/v1/users/${id}`),
  resetPassword: (id: number, b: { password: string; reset_totp: boolean }) => put<void>(`/api/v1/users/${id}/password`, b),
  mySessions: () => get<{ sessions: SessionInfo[] }>("/api/v1/account/sessions"),
  endSession: (id: string) => del<void>(`/api/v1/account/sessions/${id}`),
  endOtherSessions: () => del<void>("/api/v1/account/sessions"),
  myPasskeys: () => get<{ passkeys: Passkey[] }>("/api/v1/account/passkeys"),
  passkeyOptions: (password: string) => post<{ options: any }>("/api/v1/account/passkeys/options", { password }),
  addPasskey: (b: { name: string; client_data_json: string; attestation_object: string }) =>
    post<{ passkey: Passkey }>("/api/v1/account/passkeys", b),
  renamePasskey: (id: number, name: string) => put<void>(`/api/v1/account/passkeys/${id}`, { name }),
  deletePasskey: (id: number) => del<void>(`/api/v1/account/passkeys/${id}`),
  passkeyLoginOptions: () => post<{ challenge_id: string; options: any }>("/api/v1/auth/passkey/options"),
  passkeyLogin: (b: Record<string, string>) => post<{ user: User }>("/api/v1/auth/passkey", b),
  myTokens: () => get<{ tokens: APIToken[] }>("/api/v1/account/tokens"),
  createToken: (b: { name: string; role: Role; expires_days: number; password: string; project_id?: number }) =>
    post<{ token: APIToken; secret: string }>("/api/v1/account/tokens", b),
  deleteMyToken: (id: number) => del<void>(`/api/v1/account/tokens/${id}`),
  allTokens: () => get<{ tokens: APIToken[] }>("/api/v1/tokens"),
  revokeToken: (id: number) => del<void>(`/api/v1/tokens/${id}`),
  createUser: (b: { email: string; name: string; password: string; role: Role }) =>
    post<{ user: User }>("/api/v1/users", b),

  projects: () => get<{ projects: Project[] }>("/api/v1/projects"),
  project: (id: number) => get<{ project: Project; members: ProjectMember[]; usage: ProjectUsage }>(`/api/v1/projects/${id}`),
  setProjectLimits: (id: number, l: ProjectLimits) =>
    put<{ project: Project; usage: ProjectUsage }>(`/api/v1/projects/${id}/limits`, l),
  createProject: (b: { name: string; description: string }) => post<{ project: Project }>("/api/v1/projects", b),
  updateProject: (id: number, b: { name: string; description: string }) => put<{ project: Project }>(`/api/v1/projects/${id}`, b),
  deleteProject: (id: number) => del<void>(`/api/v1/projects/${id}`),
  addProjectMember: (id: number, email: string, role: ProjectRole) =>
    post<{ member: ProjectMember }>(`/api/v1/projects/${id}/members`, { email, role }),
  setProjectMember: (id: number, userId: number, role: ProjectRole) => put<void>(`/api/v1/projects/${id}/members/${userId}`, { role }),
  removeProjectMember: (id: number, userId: number) => del<void>(`/api/v1/projects/${id}/members/${userId}`),
  projectAudit: (id: number, before?: number) =>
    get<{ entries: AuditEntry[]; actions: string[]; more: boolean }>(`/api/v1/projects/${id}/audit${before ? `?before=${before}` : ""}`),
  moveToProject: (kind: ProjectResource, id: number, projectId: number) =>
    put<void>(`/api/v1/${kind}/${id}/project`, { project_id: projectId }),

  servers: () => get<{ servers: Server[] }>("/api/v1/servers"),
  server: (id: number) => get<{ server: Server; metrics?: Metrics }>(`/api/v1/servers/${id}`),
  createServer: (name: string) =>
    post<{ server: Server; enrollment: Enrollment }>("/api/v1/servers", { name }),
  resetEnrollToken: (id: number) =>
    post<{ enrollment: Enrollment }>(`/api/v1/servers/${id}/enroll-token`),
  deleteServer: (id: number) => del<void>(`/api/v1/servers/${id}`),
  updates: () => get<UpdatesInfo>("/api/v1/updates"),
  checkUpdates: () => post<UpdatesInfo>("/api/v1/updates/check"),
  applyUpdate: () => post<{ run: UpdatesInfo["run"] }>("/api/v1/updates/apply"),
  serverMetrics: (id: number, minutes: number) =>
    get<{ metrics: Metrics[] }>(`/api/v1/servers/${id}/metrics?minutes=${minutes}`),
  serverCertificates: (id: number) =>
    get<{
      certificates: {
        name: string;
        not_after: string | null;
        issuer?: string;
        days_left?: number;
        expiring: boolean;
        error?: string;
        error_at?: string;
      }[];
    }>(`/api/v1/servers/${id}/certificates`),
  setPublicIPs: (id: number, ips: string[]) => put<{ server: Server }>(`/api/v1/servers/${id}/public-ips`, { ips }),
  pingServer: (id: number) => post<{ rtt_ms: number; agent_time: string }>(`/api/v1/servers/${id}/ping`),

  routes: (serverId: number) => get<{ routes: Route[]; sync: Sync }>(`/api/v1/servers/${serverId}/routes`),
  createRoute: (serverId: number, b: Partial<Route>) =>
    post<{ route: Route; sync: Sync }>(`/api/v1/servers/${serverId}/routes`, b),
  deleteRoute: (serverId: number, routeId: number) =>
    del<{ sync: Sync }>(`/api/v1/servers/${serverId}/routes/${routeId}`),

  sites: () => get<{ sites: Site[] }>("/api/v1/sites"),
  site: (id: number) => get<{ site: Site; sync: Sync; placements: SitePlacement[] }>(`/api/v1/sites/${id}`),
  addSiteReplica: (id: number, serverId: number) =>
    post<{ site: Site; placements: SitePlacement[] }>(`/api/v1/sites/${id}/replicas`, { server_id: serverId }),
  removeSiteReplica: (id: number, serverId: number) =>
    del<{ site: Site; placements: SitePlacement[]; cleanup_error?: string }>(`/api/v1/sites/${id}/replicas/${serverId}`),
  syncSite: (id: number) => post<{ site: Site; placements: SitePlacement[] }>(`/api/v1/sites/${id}/sync`),
  setSitePrimary: (id: number, serverId: number, force: boolean) =>
    put<{ site: Site; placements: SitePlacement[]; warning?: string }>(`/api/v1/sites/${id}/primary`, { server_id: serverId, force }),
  createSite: (b: { server_id: number; name: string; domains: string[]; tls: string; php_version: string; project_id?: number }) =>
    post<{ site: Site; sync: Sync; provision_error?: string }>("/api/v1/sites", b),
  setSiteDomains: (id: number, domains: string[]) =>
    put<{ site: Site; sync: Sync }>(`/api/v1/sites/${id}/domains`, { domains }),
  setSitePHP: (id: number, version: string) => put<{ site: Site; sync: Sync }>(`/api/v1/sites/${id}/php`, { version }),
  setSitePHPSettings: (id: number, s: PHPSettings) => put<{ site: Site }>(`/api/v1/sites/${id}/php-settings`, s),
  installSite: (id: number, app: "wordpress") =>
    post<{ app: string; version: string; files: number; url?: string; database: Database }>(`/api/v1/sites/${id}/install`, { app }),
  measureSite: (id: number) => post<{ site: Site }>(`/api/v1/sites/${id}/usage`),
  setSiteQuota: (id: number, quota_mb: number) => put<{ site: Site }>(`/api/v1/sites/${id}/quota`, { quota_mb }),
  phpLog: (id: number) => get<{ log: string; size: number; truncated?: boolean; path: string }>(`/api/v1/sites/${id}/php-log`),
  deleteSite: (id: number) => del<{ sync: Sync; cleanup_error?: string }>(`/api/v1/sites/${id}`),

  files: (siteId: number, path: string) =>
    get<{ path: string; entries: FileEntry[] }>(`/api/v1/sites/${siteId}/files${q(path)}`),
  readText: (siteId: number, path: string) =>
    get<{ content: string; size: number }>(`/api/v1/sites/${siteId}/files/text${q(path)}`),
  fileOp: (siteId: number, op: string, b: Record<string, unknown>) =>
    post<{ ok: boolean; extract?: ExtractResult }>(`/api/v1/sites/${siteId}/files/${op}`, b),
  downloadUrl: (siteId: number, path: string) => `/api/v1/sites/${siteId}/files/content${q(path)}`,

  databases: (siteId: number) => get<{ databases: Database[]; mariadb: string }>(`/api/v1/sites/${siteId}/databases`),
  createDatabase: (siteId: number, name: string) =>
    post<{ database: Database; credentials: DatabaseCredentials }>(`/api/v1/sites/${siteId}/databases`, { name }),
  resetDatabasePassword: (siteId: number, id: number) =>
    post<{ credentials: DatabaseCredentials }>(`/api/v1/sites/${siteId}/databases/${id}/password`),
  deleteDatabase: (siteId: number, id: number) => del<void>(`/api/v1/sites/${siteId}/databases/${id}`),

  registries: () => get<{ registries: Registry[] }>("/api/v1/registries"),
  createRegistry: (b: { host: string; username: string; password: string }) =>
    post<{ registry: Registry }>("/api/v1/registries", b),
  updateRegistry: (id: number, b: { username: string; password: string }) =>
    put<{ registry: Registry }>(`/api/v1/registries/${id}`, b),
  deleteRegistry: (id: number) => del<void>(`/api/v1/registries/${id}`),

  dns: () => get<{ zones: DNSZone[]; nameservers: string[]; servers: DNSServer[] }>("/api/v1/dns"),
  setNameservers: (nameservers: string[]) => put<{ nameservers: string[] }>("/api/v1/dns/nameservers", { nameservers }),
  createZone: (name: string, projectId?: number) => post<{ zone: DNSZone }>("/api/v1/dns/zones", { name, project_id: projectId }),
  zone: (id: number) =>
    get<{ zone: DNSZone; records: DNSRecord[]; auto: DNSAutoRecord[]; types: string[] }>(`/api/v1/dns/zones/${id}`),
  deleteZone: (id: number) => del<void>(`/api/v1/dns/zones/${id}`),
  setDNSSEC: (id: number, enabled: boolean) => put<{ zone: DNSZone }>(`/api/v1/dns/zones/${id}/dnssec`, { enabled }),
  createRecord: (zoneId: number, r: DNSRecordInput) => post<{ record: DNSRecord }>(`/api/v1/dns/zones/${zoneId}/records`, r),
  updateRecord: (zoneId: number, id: number, r: DNSRecordInput) =>
    put<{ record: DNSRecord }>(`/api/v1/dns/zones/${zoneId}/records/${id}`, r),
  deleteRecord: (zoneId: number, id: number) => del<void>(`/api/v1/dns/zones/${zoneId}/records/${id}`),

  catalog: () => get<{ templates: Template[]; source: CatalogSource | null }>("/api/v1/catalog"),
  setCatalogSource: (b: { url: string; key: string; allow_private: boolean }) =>
    put<{ source: CatalogSource | null }>("/api/v1/catalog/source", b),
  refreshCatalog: () => post<{ source: CatalogSource | null }>("/api/v1/catalog/refresh"),
  installTemplate: (id: string, b: { server_id: number; name: string; inputs: Record<string, string>; project_id?: number }) =>
    post<{ app: App; notes: string; deploy_error?: string }>(`/api/v1/catalog/${id}/install`, b),
  appTemplate: (id: number) => get<AppTemplateInfo>(`/api/v1/apps/${id}/template`),
  updateTemplate: (id: number, b: { inputs: Record<string, string>; backup: boolean }) =>
    post<{ update: AppTemplateInfo["update"] }>(`/api/v1/apps/${id}/template-update`, b),
  audit: (q: { before?: number; actor?: string; action?: string }) => {
    const p = new URLSearchParams();
    if (q.before) p.set("before", String(q.before));
    if (q.actor) p.set("actor", q.actor);
    if (q.action) p.set("action", q.action);
    return get<{ entries: AuditEntry[]; actions: string[]; more: boolean }>(`/api/v1/audit?${p}`);
  },
  openapi: () => get<OpenAPISpec>("/api/v1/openapi.json"),
  verifyAudit: () => get<AuditCheck>("/api/v1/audit/verify"),
  healthChecks: () => get<{ checks: HealthCheck[] }>("/api/v1/health-checks"),
  createHealthCheck: (b: HealthCheckInput) => post<{ check: { id: number } }>("/api/v1/health-checks", b),
  updateHealthCheck: (id: number, b: HealthCheckInput) => put<void>(`/api/v1/health-checks/${id}`, b),
  deleteHealthCheck: (id: number) => del<void>(`/api/v1/health-checks/${id}`),
  healthEvents: (id: number) =>
    get<{ events: { status: string; detail: string; at: string }[] }>(`/api/v1/health-checks/${id}/events`),
  notifications: () =>
    get<{ channels: NotificationChannel[]; events: NotificationEvent[]; panel_url: string }>("/api/v1/notifications"),
  setPanelURL: (url: string) => put<{ panel_url: string }>("/api/v1/notifications/panel-url", { url }),
  createChannel: (b: ChannelInput) => post<{ channel: NotificationChannel }>("/api/v1/notifications", b),
  updateChannel: (id: number, b: ChannelInput) => put<{ channel: NotificationChannel }>(`/api/v1/notifications/${id}`, b),
  deleteChannel: (id: number) => del<void>(`/api/v1/notifications/${id}`),
  testChannel: (id: number) => post<{ ok: boolean; error?: string }>(`/api/v1/notifications/${id}/test`),
  mail: () => get<MailOverview>("/api/v1/mail"),
  createMail: (b: { server_id: number; hostname: string; memory_mb: number }) =>
    post<{ server: App; deploy_error?: string }>("/api/v1/mail", b),
  updateMailSettings: (b: { relay?: MailRelayInput | null; send_limit?: number }) =>
    put<{ settings: MailSettings; warning?: string }>("/api/v1/mail/settings", b),
  updateMailDomain: (id: number, b: { catch_all: string }) => put<void>(`/api/v1/mail/domains/${id}`, b),
  forwarders: (domainId: number) => get<{ forwarders: Forwarder[] }>(`/api/v1/mail/domains/${domainId}/forwarders`),
  createForwarder: (domainId: number, b: { local: string; recipients: string[] }) =>
    post<{ forwarder: Forwarder }>(`/api/v1/mail/domains/${domainId}/forwarders`, b),
  updateForwarder: (domainId: number, id: string, recipients: string[]) =>
    put<void>(`/api/v1/mail/domains/${domainId}/forwarders/${encodeURIComponent(id)}`, { recipients }),
  deleteForwarder: (domainId: number, id: string) =>
    del<void>(`/api/v1/mail/domains/${domainId}/forwarders/${encodeURIComponent(id)}`),
  mailChecks: () => post<{ checks: MailChecks }>("/api/v1/mail/checks", {}),
  createMailDomain: (name: string) => post<{ domain: MailDomain }>("/api/v1/mail/domains", { name }),
  deleteMailDomain: (id: number) => del<void>(`/api/v1/mail/domains/${id}`),
  mailboxes: (domainId: number) => get<{ mailboxes: Mailbox[] }>(`/api/v1/mail/domains/${domainId}/mailboxes`),
  createMailbox: (domainId: number, b: MailboxInput) =>
    post<{ mailbox: { id: string; address: string } }>(`/api/v1/mail/domains/${domainId}/mailboxes`, b),
  updateMailbox: (domainId: number, id: string, b: MailboxInput) =>
    put<void>(`/api/v1/mail/domains/${domainId}/mailboxes/${encodeURIComponent(id)}`, b),
  deleteMailbox: (domainId: number, id: string) =>
    del<void>(`/api/v1/mail/domains/${domainId}/mailboxes/${encodeURIComponent(id)}`),

  apps: () => get<{ apps: App[] }>("/api/v1/apps"),
  app: (id: number) => get<{ app: App; sync: Sync; placements: AppPlacement[] }>(`/api/v1/apps/${id}`),
  addReplica: (id: number, serverId: number) =>
    post<{ app?: App; deploying: boolean; deploy_error?: string }>(`/api/v1/apps/${id}/replicas`, { server_id: serverId }),
  removeReplica: (id: number, serverId: number) =>
    del<{ app?: App; cleanup_error?: string }>(`/api/v1/apps/${id}/replicas/${serverId}`),
  createApp: (
    b: AppSettings & {
      source: string;
      server_id: number;
      name: string;
      domains: string[];
      tls: string;
      compose?: string;
      service?: string;
      engine?: string;
      version?: string;
      project_id?: number;
    },
  ) => post<{ app: App; sync: Sync; warnings: string[] }>("/api/v1/apps", b),
  databaseEngines: () => get<{ engines: Engine[] }>("/api/v1/database-engines"),
  databaseConnection: (id: number) => get<{ connection: DatabaseConnection }>(`/api/v1/apps/${id}/connection`),
  databasePassword: (id: number, redeploy_apps: boolean) =>
    post<{ connection: DatabaseConnection; updated_apps: string[]; redeploying: string[]; deploy_error?: string }>(
      `/api/v1/apps/${id}/password`,
      { redeploy_apps },
    ),
  linkApp: (id: number, appId: number, envVar: string) =>
    post<{ app: App; attach_error?: string }>(`/api/v1/apps/${id}/links`, { app_id: appId, env_var: envVar }),
  unlinkApp: (id: number, appId: number) => del<{ app: App; attach_error?: string }>(`/api/v1/apps/${id}/links/${appId}`),
  appCompose: (id: number) => get<ComposeFile>(`/api/v1/apps/${id}/compose`),
  setAppCompose: (id: number, b: { compose: string; service: string; port: number }) =>
    put<{ app: App; warnings: string[] }>(`/api/v1/apps/${id}/compose`, b),
  updateApp: (id: number, b: AppSettings) => put<{ app: App }>(`/api/v1/apps/${id}`, b),
  deleteApp: (id: number, volumes: boolean) =>
    del<{ sync: Sync; cleanup_error?: string }>(`/api/v1/apps/${id}${volumes ? "?volumes=1" : ""}`),
  setAppDomains: (id: number, domains: string[]) =>
    put<{ app: App; sync: Sync }>(`/api/v1/apps/${id}/domains`, { domains }),
  regenerateDeployKey: (id: number) => post<{ app: App }>(`/api/v1/apps/${id}/deploy-key`),
  appWebhook: (id: number) => get<{ webhook: Webhook }>(`/api/v1/apps/${id}/webhook`),
  setAppWebhook: (id: number, b: { auto_deploy: boolean; regenerate?: boolean }) =>
    put<{ webhook: Webhook }>(`/api/v1/apps/${id}/webhook`, b),
  appEnv: (id: number) => get<{ env: Record<string, string> }>(`/api/v1/apps/${id}/env`),
  setAppEnv: (id: number, env: Record<string, string>) => put<{ ok: boolean }>(`/api/v1/apps/${id}/env`, { env }),
  deployApp: (id: number, rollbackTo?: number) =>
    post<{ deployment: Deployment }>(`/api/v1/apps/${id}/deploy`, rollbackTo ? { rollback_to: rollbackTo } : {}),
  cancelDeployment: (id: number, did: number) => post<Record<string, never>>(`/api/v1/apps/${id}/deployments/${did}/cancel`),
  deployments: (id: number) => get<{ deployments: Deployment[] }>(`/api/v1/apps/${id}/deployments`),
  deployment: (id: number, did: number) => get<{ deployment: Deployment }>(`/api/v1/apps/${id}/deployments/${did}`),
  appMetrics: (id: number, minutes: number) => get<{ metrics: AppMetrics[] }>(`/api/v1/apps/${id}/metrics?minutes=${minutes}`),
  appStatus: (id: number) => get<{ status: AppStatus }>(`/api/v1/apps/${id}/status`),
  appLogs: (id: number, tail: number, service = "", server = 0) =>
    get<{ log: string }>(`/api/v1/apps/${id}/logs?tail=${tail}${service ? `&service=${encodeURIComponent(service)}` : ""}${server ? `&server=${server}` : ""}`),
  // Server-sent event streams (see useEventStream).
  appLogsStreamUrl: (id: number, tail: number, service = "", server = 0) =>
    `/api/v1/apps/${id}/logs/stream?tail=${tail}${service ? `&service=${encodeURIComponent(service)}` : ""}${server ? `&server=${server}` : ""}`,
  deploymentStreamUrl: (id: number, did: number) => `/api/v1/apps/${id}/deployments/${did}/stream`,

  backupStorages: () => get<{ storages: BackupStorage[] }>("/api/v1/backup-storages"),
  createBackupStorage: (b: StorageInput) => post<{ storage: BackupStorage }>("/api/v1/backup-storages", b),
  updateBackupStorage: (id: number, b: StorageInput) => put<{ storage: BackupStorage }>(`/api/v1/backup-storages/${id}`, b),
  deleteBackupStorage: (id: number) => del<{ ok: boolean }>(`/api/v1/backup-storages/${id}`),
  backupChecks: () => get<{ repositories: CheckRepository[] }>("/api/v1/backup-checks"),
  startBackupCheck: (storageId: number, serverId: number) =>
    post<{ check: BackupCheck }>("/api/v1/backup-checks", { storage_id: storageId, server_id: serverId }),
  backupCheck: (id: number) => get<{ check: BackupCheck }>(`/api/v1/backup-checks/${id}`),
  backupStoragePassword: (id: number) => get<{ password: string }>(`/api/v1/backup-storages/${id}/password`),
  checkBackupStorage: (id: number, serverId: number) =>
    post<{ ok: boolean; error?: string }>(`/api/v1/backup-storages/${id}/check`, { server_id: serverId }),
  // Backups of a site or app; base is "/api/v1/sites/1" or "/api/v1/apps/1".
  backups: (base: string) =>
    get<{ policy: BackupPolicy | null; backups: BackupRun[]; restic: string; items: BackupItem[] }>(`${base}/backups`),
  backup: (base: string, id: number) => get<{ backup: BackupRun }>(`${base}/backups/${id}`),
  backupStreamUrl: (base: string, id: number) => `${base}/backups/${id}/stream`,
  setBackupPolicy: (url: string, b: Omit<BackupPolicy, "next_run">) => put<{ policy: BackupPolicy }>(url, b),
  deleteBackupPolicy: (url: string) => del<{ ok: boolean }>(url),
  panelBackup: () =>
    get<{
      recovery: { set: boolean; set_at?: string };
      policy: BackupPolicy | null;
      backups: PanelBackupRun[];
      restic: string;
      restic_error?: string;
    }>("/api/v1/panel-backup"),
  setRecoveryPassphrase: (password: string, passphrase: string) =>
    put<{ ok: boolean }>("/api/v1/panel-backup/recovery", { password, passphrase }),
  runPanelBackup: () => post<{ backup: PanelBackupRun }>("/api/v1/panel-backup/run"),
  panelBackupRun: (id: number) => get<{ backup: PanelBackupRun }>(`/api/v1/panel-backup/runs/${id}`),
  runBackup: (base: string) => post<{ backup: BackupRun }>(`${base}/backups`),
  snapshots: (base: string) => get<{ snapshots: Snapshot[] }>(`${base}/snapshots`),
  restore: (base: string, snapshots: string[]) => post<{ backup: BackupRun }>(`${base}/restore`, { snapshots }),
  // Files of a backup; base is "/api/v1/sites/1" or "/api/v1/apps/1", volume
  // names an app's volume.
  snapshotFiles: (base: string, snap: string, path: string, volume = "") =>
    get<{ path: string; entries: SnapshotEntry[] }>(`${base}/snapshots/${snap}/files${q(path)}${volume ? `&volume=${encodeURIComponent(volume)}` : ""}`),
  snapshotDownloadUrl: (base: string, snap: string, path: string, volume = "") =>
    `${base}/snapshots/${snap}/download${q(path)}${volume ? `&volume=${encodeURIComponent(volume)}` : ""}`,
  dumpDownloadUrl: (base: string, snap: string, database: string) =>
    `${base}/snapshots/${snap}/download?database=${encodeURIComponent(database)}`,
  restoreCopy: (base: string, snap: string, path: string, volume = "") =>
    post<RestoredCopy>(`${base}/snapshots/${snap}/restore-copy`, volume ? { path, volume } : { path }),

  sftpUsers: (siteId: number) =>
    get<{ users: SFTPUser[]; connection: SFTPConnection | null }>(`/api/v1/sites/${siteId}/sftp-users`),
  createSFTPUser: (siteId: number, b: { username: string; password?: string; public_keys?: string[] }) =>
    post<{ user: SFTPUser; password?: string; connection: SFTPConnection | null; sync_error?: string }>(
      `/api/v1/sites/${siteId}/sftp-users`,
      b,
    ),
  updateSFTPUser: (siteId: number, id: number, b: { password?: string; public_keys?: string[] }) =>
    put<{ user: SFTPUser; password?: string; sync_error?: string }>(`/api/v1/sites/${siteId}/sftp-users/${id}`, b),
  deleteSFTPUser: (siteId: number, id: number) => del<{ ok: boolean }>(`/api/v1/sites/${siteId}/sftp-users/${id}`),
  accessLog: (base: string) => get<{ entries: AccessEntry[] }>(`${base}/access-log?limit=200`),
  trash: (serverId: number) => get<{ entries: TrashEntry[]; keep_days: number }>(`/api/v1/servers/${serverId}/trash`),
  removeTrash: (serverId: number, name: string) =>
    del<{ entries: TrashEntry[]; keep_days: number }>(`/api/v1/servers/${serverId}/trash/${encodeURIComponent(name)}`),
  sftpBans: (serverId: number) => get<{ bans: SFTPBan[] }>(`/api/v1/servers/${serverId}/sftp-bans`),
  sftpUnban: (serverId: number, ip: string) => del<{ bans: SFTPBan[] }>(`/api/v1/servers/${serverId}/sftp-bans/${encodeURIComponent(ip)}`),
};

// upload sends one file with progress reporting (fetch has no upload progress).
export function upload(
  siteId: number,
  path: string,
  file: Blob,
  onProgress: (fraction: number) => void,
  signal?: AbortSignal,
): Promise<void> {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open("PUT", `/api/v1/sites/${siteId}/files/content${q(path)}`);
    xhr.upload.onprogress = (e) => e.lengthComputable && onProgress(e.loaded / e.total);
    xhr.onload = () => {
      if (xhr.status >= 200 && xhr.status < 300) return resolve();
      let msg = xhr.statusText;
      try {
        msg = JSON.parse(xhr.responseText).error ?? msg;
      } catch {}
      reject(new ApiError(xhr.status, msg));
    };
    xhr.onerror = () => reject(new ApiError(0, "network error"));
    signal?.addEventListener("abort", () => xhr.abort());
    xhr.send(file);
  });
}

export const engineNames: Record<string, string> = { postgres: "PostgreSQL", mysql: "MySQL", mariadb: "MariaDB", redis: "Redis" };

// appPath is the page of an app or database.
export const appPath = (a: Pick<App, "id" | "source">) => `/${a.source === "database" ? "databases" : "apps"}/${a.id}`;

// sourceLabel describes where an app comes from, for lists and headers.
export const sourceLabel = (a: App) =>
  a.source === "mail"
    ? `mail server · ${a.image}`
    : a.source === "database"
    ? `${engineNames[a.engine ?? ""] ?? a.engine} ${a.version}`
    : a.source === "git"
    ? `${a.git?.url} · ${a.git?.branch}`
    : a.source === "compose" && a.template
      ? `catalog · ${a.template} · ${(a.compose?.services ?? []).map((s) => s.name).join(", ")}`
    : a.source === "compose"
      ? `compose · ${(a.compose?.services ?? []).map((s) => s.name).join(", ")}`
      : a.image;

export const roleRank: Record<Role, number> = { user: 0, viewer: 1, member: 2, admin: 3, owner: 4 };
export const can = (user: User | undefined, min: Role) => !!user && roleRank[user.role] >= roleRank[min];

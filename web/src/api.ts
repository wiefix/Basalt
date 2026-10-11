// Typed client for the Basalt API. Types mirror the JSON views in
// internal/server/*_handlers.go.

export type Role = "owner" | "admin" | "member" | "viewer";

export interface User {
  id: number;
  email: string;
  name: string;
  role: Role;
  totp_enabled: boolean;
  disabled: boolean;
  created_at: string;
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
  name: string;
  kind: "static";
  domains: string[];
  tls: "auto" | "internal";
  php_version: string;
  created_at: string;
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
  created_at: string;
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

export interface Snapshot {
  id: string;
  time: string;
  item: BackupItem;
  bytes: number;
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
  secrets: { key: string; length: number }[] | null;
  expose: { service: string; port: number };
  notes: string;
  services: { name: string; image: string }[];
  volumes: string[] | null;
  compose: string;
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
  createUser: (b: { email: string; name: string; password: string; role: Role }) =>
    post<{ user: User }>("/api/v1/users", b),

  servers: () => get<{ servers: Server[] }>("/api/v1/servers"),
  server: (id: number) => get<{ server: Server; metrics?: Metrics }>(`/api/v1/servers/${id}`),
  createServer: (name: string) =>
    post<{ server: Server; enrollment: Enrollment }>("/api/v1/servers", { name }),
  resetEnrollToken: (id: number) =>
    post<{ enrollment: Enrollment }>(`/api/v1/servers/${id}/enroll-token`),
  deleteServer: (id: number) => del<void>(`/api/v1/servers/${id}`),
  serverMetrics: (id: number, minutes: number) =>
    get<{ metrics: Metrics[] }>(`/api/v1/servers/${id}/metrics?minutes=${minutes}`),
  setPublicIPs: (id: number, ips: string[]) => put<{ server: Server }>(`/api/v1/servers/${id}/public-ips`, { ips }),
  pingServer: (id: number) => post<{ rtt_ms: number; agent_time: string }>(`/api/v1/servers/${id}/ping`),

  routes: (serverId: number) => get<{ routes: Route[]; sync: Sync }>(`/api/v1/servers/${serverId}/routes`),
  createRoute: (serverId: number, b: Partial<Route>) =>
    post<{ route: Route; sync: Sync }>(`/api/v1/servers/${serverId}/routes`, b),
  deleteRoute: (serverId: number, routeId: number) =>
    del<{ sync: Sync }>(`/api/v1/servers/${serverId}/routes/${routeId}`),

  sites: () => get<{ sites: Site[] }>("/api/v1/sites"),
  site: (id: number) => get<{ site: Site; sync: Sync }>(`/api/v1/sites/${id}`),
  createSite: (b: { server_id: number; name: string; domains: string[]; tls: string; php_version: string }) =>
    post<{ site: Site; sync: Sync; provision_error?: string }>("/api/v1/sites", b),
  setSiteDomains: (id: number, domains: string[]) =>
    put<{ site: Site; sync: Sync }>(`/api/v1/sites/${id}/domains`, { domains }),
  setSitePHP: (id: number, version: string) => put<{ site: Site; sync: Sync }>(`/api/v1/sites/${id}/php`, { version }),
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
  createZone: (name: string) => post<{ zone: DNSZone }>("/api/v1/dns/zones", { name }),
  zone: (id: number) =>
    get<{ zone: DNSZone; records: DNSRecord[]; auto: DNSAutoRecord[]; types: string[] }>(`/api/v1/dns/zones/${id}`),
  deleteZone: (id: number) => del<void>(`/api/v1/dns/zones/${id}`),
  setDNSSEC: (id: number, enabled: boolean) => put<{ zone: DNSZone }>(`/api/v1/dns/zones/${id}/dnssec`, { enabled }),
  createRecord: (zoneId: number, r: DNSRecordInput) => post<{ record: DNSRecord }>(`/api/v1/dns/zones/${zoneId}/records`, r),
  updateRecord: (zoneId: number, id: number, r: DNSRecordInput) =>
    put<{ record: DNSRecord }>(`/api/v1/dns/zones/${zoneId}/records/${id}`, r),
  deleteRecord: (zoneId: number, id: number) => del<void>(`/api/v1/dns/zones/${zoneId}/records/${id}`),

  catalog: () => get<{ templates: Template[] }>("/api/v1/catalog"),
  installTemplate: (id: string, b: { server_id: number; name: string; inputs: Record<string, string> }) =>
    post<{ app: App; notes: string; deploy_error?: string }>(`/api/v1/catalog/${id}/install`, b),
  mail: () => get<MailOverview>("/api/v1/mail"),
  createMail: (b: { server_id: number; hostname: string; memory_mb: number }) =>
    post<{ server: App; deploy_error?: string }>("/api/v1/mail", b),
  updateMailSettings: (b: { relay?: MailRelayInput | null; send_limit?: number }) =>
    put<{ settings: MailSettings }>("/api/v1/mail/settings", b),
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
  app: (id: number) => get<{ app: App; sync: Sync }>(`/api/v1/apps/${id}`),
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
    },
  ) => post<{ app: App; sync: Sync; warnings: string[] }>("/api/v1/apps", b),
  databaseEngines: () => get<{ engines: Engine[] }>("/api/v1/database-engines"),
  databaseConnection: (id: number) => get<{ connection: DatabaseConnection }>(`/api/v1/apps/${id}/connection`),
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
  deployments: (id: number) => get<{ deployments: Deployment[] }>(`/api/v1/apps/${id}/deployments`),
  deployment: (id: number, did: number) => get<{ deployment: Deployment }>(`/api/v1/apps/${id}/deployments/${did}`),
  appMetrics: (id: number, minutes: number) => get<{ metrics: AppMetrics[] }>(`/api/v1/apps/${id}/metrics?minutes=${minutes}`),
  appStatus: (id: number) => get<{ status: AppStatus }>(`/api/v1/apps/${id}/status`),
  appLogs: (id: number, tail: number, service = "") =>
    get<{ log: string }>(`/api/v1/apps/${id}/logs?tail=${tail}${service ? `&service=${encodeURIComponent(service)}` : ""}`),
  // Server-sent event streams (see useEventStream).
  appLogsStreamUrl: (id: number, tail: number, service = "") =>
    `/api/v1/apps/${id}/logs/stream?tail=${tail}${service ? `&service=${encodeURIComponent(service)}` : ""}`,
  deploymentStreamUrl: (id: number, did: number) => `/api/v1/apps/${id}/deployments/${did}/stream`,

  backupStorages: () => get<{ storages: BackupStorage[] }>("/api/v1/backup-storages"),
  createBackupStorage: (b: StorageInput) => post<{ storage: BackupStorage }>("/api/v1/backup-storages", b),
  updateBackupStorage: (id: number, b: StorageInput) => put<{ storage: BackupStorage }>(`/api/v1/backup-storages/${id}`, b),
  deleteBackupStorage: (id: number) => del<{ ok: boolean }>(`/api/v1/backup-storages/${id}`),
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

  sftpUsers: (siteId: number) =>
    get<{ users: SFTPUser[]; connection: SFTPConnection | null }>(`/api/v1/sites/${siteId}/sftp-users`),
  createSFTPUser: (siteId: number, b: { username: string; password?: string; public_keys?: string[] }) =>
    post<{ user: SFTPUser; password?: string; connection: SFTPConnection | null; sync_error?: string }>(
      `/api/v1/sites/${siteId}/sftp-users`,
      b,
    ),
  deleteSFTPUser: (siteId: number, id: number) => del<{ ok: boolean }>(`/api/v1/sites/${siteId}/sftp-users/${id}`),
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

export const roleRank: Record<Role, number> = { viewer: 1, member: 2, admin: 3, owner: 4 };
export const can = (user: User | undefined, min: Role) => !!user && roleRank[user.role] >= roleRank[min];

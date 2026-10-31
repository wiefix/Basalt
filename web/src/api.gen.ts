// Generated from the API description (/api/v1/openapi.json) by TestTypeScriptTypes in
// internal/server: run `make api-types` after changing the API. Do not edit.
//
// <Operation>Response is what a route answers, <Operation>Body what it reads;
// api.check.ts checks the client in api.ts against them.

export interface AddMemberRequest {
  email?: string;
  role?: string;
}

export interface AddPasskeyRequest {
  attestation_object?: string;
  client_data_json?: string;
  name?: string;
}

export interface AdminerView {
  installed: boolean;
  path?: string;
  url?: string;
  version?: string;
}

export interface AgentprotoAccessEntry {
  at: string;
  bytes: number;
  duration_ms: number;
  host: string;
  ip: string;
  method: string;
  proto: string;
  status: number;
  uri: string;
  user_agent?: string;
}

export interface AgentprotoAppStatus {
  deployment_id?: number;
  error?: string;
  exit_code?: number;
  health?: string;
  oom_killed?: boolean;
  restart_count?: number;
  services?: AgentprotoServiceStatus[];
  started_at?: string;
  state: string;
}

export interface AgentprotoAppVolume {
  name?: string;
  path?: string;
}

export interface AgentprotoAppVolumeOut {
  name: string;
  path: string;
}

export interface AgentprotoBackupItem {
  app_id?: number;
  database?: string;
  engine?: string;
  kind: string;
  service?: string;
  site_id?: number;
  user?: string;
  volume?: string;
}

export interface AgentprotoMailCheckResult {
  check: string;
  detail: string;
  domain?: string;
  status: string;
  target?: string;
}

export interface AgentprotoRestoreCopyResponse {
  bytes: number;
  dirs: number;
  files: number;
  folder: string;
  skipped: number;
}

export interface AgentprotoServiceStatus {
  deployment_id?: number;
  exit_code?: number;
  health?: string;
  image: string;
  name: string;
  oom_killed?: boolean;
  restart_count?: number;
  started_at?: string;
  state: string;
}

export interface AgentprotoSnapshotEntry {
  mod_time: string;
  mode: string;
  name: string;
  size: number;
  type: string;
}

export interface AgentprotoSnapshotInfo {
  bytes: number;
  id: string;
  item: AgentprotoBackupItem;
  time: string;
}

export interface AgentprotoTrashEntry {
  bytes: number;
  deleted_at: string;
  name: string;
  site_id: number;
}

export interface AppDomainsRequest {
  domains?: string[];
}

export interface AppEnvRequest {
  env?: Record<string, string>;
}

export interface AppMetricsView {
  at: string;
  cpu_pct: number;
  mem_limit: number;
  mem_used: number;
  net_rx: number;
  net_tx: number;
}

export interface AppRoutesRequest {
  routes?: StoreAppRoute[];
}

export interface AppSettingsRequest {
  cpus?: number;
  git?: GitView;
  image?: string;
  memory_mb?: number;
  port?: number;
  read_only?: boolean;
  volumes?: AgentprotoAppVolume[];
}

export interface AppView {
  auto_deploy: boolean;
  compose?: StoreComposeSummary;
  cpus: number;
  created_at: string;
  deploy_key?: string;
  domains: string[];
  engine?: string;
  git?: GitViewOut;
  id: number;
  image: string;
  links?: number[];
  memory_mb: number;
  name: string;
  port: number;
  pr_number?: number;
  preview_domain?: string;
  preview_of?: number;
  project_id: number;
  read_only: boolean;
  replicas: number[];
  routes?: StoreAppRouteOut[];
  server_id: number;
  service?: string;
  source: string;
  template?: string;
  template_revision?: number;
  template_update?: string;
  tls: string;
  version?: string;
  volumes: AgentprotoAppVolumeOut[];
}

export interface AuditActor {
  email?: string;
  id: number;
  name?: string;
}

export interface AuditAnchorView {
  at?: string;
  checked_at: string;
  entry: number;
  ok: boolean;
  problem?: string;
  server: string;
  server_id: number;
}

export interface AuditCheckView {
  anchors: AuditAnchorView[];
  at: string;
  broken_at?: number;
  entries: number;
  first_id?: number;
  intact: boolean;
  legacy: number;
  pruned: number;
}

export interface AuditRetentionRequest {
  days?: number;
}

export interface AuditRetentionView {
  days: number;
  first_id?: number;
  pruned: number;
  pruned_at?: string;
}

export interface AuditView {
  action: string;
  actor: AuditActor | null;
  at: string;
  details: Record<string, unknown>;
  id: number;
  ip?: string;
}

export interface BackupPartView {
  bytes?: number;
  data_added?: number;
  id?: string;
  item: AgentprotoBackupItem;
  snapshot?: string;
}

export interface BackupView {
  bytes: number;
  created_at: string;
  data_added: number;
  error?: string;
  finished_at?: string;
  id: number;
  kind: string;
  log?: string;
  snapshots: BackupPartView[];
  status: string;
  storage_id?: number;
  trigger: string;
}

export interface CatalogExpose {
  port: number;
  service: string;
}

export interface CatalogInput {
  default?: string;
  help?: string;
  key: string;
  label: string;
  required: boolean;
  type: string;
}

export interface CatalogSecret {
  charset?: string;
  key: string;
  length: number;
}

export interface CatalogService {
  image: string;
  name: string;
}

export interface CatalogSourceRequest {
  allow_private?: boolean;
  key?: string;
  url?: string;
}

export interface CatalogSourceView {
  allow_private: boolean;
  error?: string;
  error_at?: string;
  fetched_at?: string;
  generated?: string;
  key: string;
  skipped: string[];
  templates: number;
  url: string;
}

export interface CertView {
  days_left?: number;
  error?: string;
  error_at?: string;
  expiring: boolean;
  issuer?: string;
  name: string;
  not_after: string | null;
}

export interface ChangePasswordRequest {
  current_password?: string;
  new_password?: string;
}

export interface ChannelRequest {
  allow_private?: boolean;
  chat_id?: string;
  events?: string[];
  from?: string;
  kind?: string;
  name?: string;
  project_id?: number;
  secret?: string;
  smtp_host?: string;
  smtp_insecure?: boolean;
  smtp_password?: string;
  smtp_port?: number;
  smtp_tls?: string;
  smtp_user?: string;
  to?: string[];
  token?: string;
  url?: string;
}

export interface ChannelView {
  allow_private?: boolean;
  chat_id?: string;
  events: string[];
  from?: string;
  host?: string;
  id: number;
  kind: string;
  last_error?: string;
  last_error_at?: string;
  last_sent_at: string | null;
  name: string;
  project_id: number;
  queued: number;
  secret?: string;
  smtp_host?: string;
  smtp_insecure?: boolean;
  smtp_port?: number;
  smtp_tls?: string;
  smtp_user?: string;
  to?: string[];
  url?: string;
}

export interface CheckRepoView {
  last: CheckView | null;
  next_at?: string;
  server: string;
  server_id: number;
  storage_id: number;
}

export interface CheckStorageRequest {
  server_id?: number;
}

export interface CheckView {
  created_at: string;
  error?: string;
  finished_at?: string;
  id: number;
  log?: string;
  read_data?: string;
  server_id?: number;
  status: string;
  storage_id: number;
  trigger: string;
}

export interface CodeRequest {
  code?: string;
}

export interface ComposeRequest {
  compose?: string;
  port?: number;
  service?: string;
}

export interface ConnectionView {
  credentials: DbengineCredentials;
  env_var: string;
  host: string;
  port: number;
  public: PublicAccessView | null;
  public_tls: boolean;
  url: string;
}

export interface CreateAppRequest {
  compose?: string;
  cpus?: number;
  domains?: string[];
  engine?: string;
  git?: GitView;
  image?: string;
  memory_mb?: number;
  name?: string;
  port?: number;
  project_id?: number;
  read_only?: boolean;
  server_id?: number;
  service?: string;
  source?: string;
  tls?: string;
  version?: string;
  volumes?: AgentprotoAppVolume[];
}

export interface CreateDatabaseRequest {
  name?: string;
}

export interface CreateMailRequest {
  hostname?: string;
  memory_mb?: number;
  server_id?: number;
}

export interface CreateRouteRequest {
  domains?: string[];
  hsts?: boolean;
  kind?: string;
  root?: string;
  tls?: string;
  upstream?: string;
}

export interface CreateSFTPUserRequest {
  password?: string;
  public_keys?: string[];
  username?: string;
}

export interface CreateServerRequest {
  name?: string;
}

export interface CreateSiteRequest {
  domains?: string[];
  name?: string;
  php_version?: string;
  project_id?: number;
  server_id?: number;
  tls?: string;
}

export interface CreateStandbyRequest {
  name?: string;
}

export interface CreateTokenRequest {
  expires_days?: number;
  name?: string;
  password?: string;
  project_id?: number;
  role?: string;
}

export interface CreateUserRequest {
  email?: string;
  name?: string;
  password?: string;
  role?: string;
}

export interface CreateZoneRequest {
  name?: string;
  project_id?: number;
}

export interface DatabaseView {
  created_at: string;
  id: number;
  name: string;
  username: string;
}

export interface DbCopyView {
  copied: string[];
  error?: string;
  lag: number;
  running: boolean;
  updated_at: string;
  waiting: string[];
}

export interface DbCredentialsView {
  database: string;
  host: string;
  password: string;
  username: string;
}

export interface DbPasswordRequest {
  redeploy_apps?: boolean;
}

export interface DbUpgrade {
  backup_id?: number;
  error?: string;
  state: string;
  version: string;
}

export interface DbUpgradeRequest {
  backup?: boolean;
  version?: string;
}

export interface DbUpgradeStarted {
  deployment: DeploymentView | null;
  upgrade: DbUpgrade | null;
}

export interface DbUpgradeView {
  backups: boolean;
  dump: boolean;
  targets: string[];
  upgrade: DbUpgrade | null;
  version: string;
}

export interface DbengineCredentials {
  database?: string;
  password: string;
  root_password?: string;
  user?: string;
}

export interface DbengineEngine {
  env_var: string;
  id: string;
  name: string;
  port: number;
  sql: boolean;
  versions: string[];
}

export interface DeployRequest {
  rollback_to?: number;
}

export interface DeploymentView {
  commit?: string;
  commit_message?: string;
  created_at: string;
  digests?: Record<string, string>;
  error?: string;
  finished_at?: string;
  id: number;
  image: string;
  image_digest?: string;
  log?: string;
  status: string;
}

export interface DisableTOTPRequest {
  code?: string;
  password?: string;
}

export interface DnsAutoRecord {
  flags?: number;
  health_check?: number;
  name: string;
  port?: number;
  priority?: number;
  source: string;
  tag?: string;
  ttl: number;
  type: string;
  value: string;
  weight?: number;
}

export interface DnsRecord {
  flags?: number;
  health_check?: number;
  name?: string;
  port?: number;
  priority?: number;
  tag?: string;
  ttl?: number;
  type?: string;
  value?: string;
  weight?: number;
}

export interface DnsRecordOut {
  flags?: number;
  health_check?: number;
  name: string;
  port?: number;
  priority?: number;
  tag?: string;
  ttl: number;
  type: string;
  value: string;
  weight?: number;
}

export interface DnsRecordView {
  flags?: number;
  health_check?: number;
  id: number;
  name: string;
  port?: number;
  priority?: number;
  tag?: string;
  ttl: number;
  type: string;
  value: string;
  weight?: number;
}

export interface DnsServerView {
  error?: string;
  in_sync: boolean;
  ip?: string;
  listen_error?: string;
  name: string;
  online: boolean;
  port: number;
  server_id: number;
}

export interface DnsZoneView {
  created_at: string;
  dnssec?: DnssecView;
  id: number;
  name: string;
  project_id: number;
  records: number;
  serial: number;
  updated_at: string;
}

export interface DnssecRequest {
  enabled?: boolean;
}

export interface DnssecView {
  algorithm: number;
  dnskey: string;
  ds: string[];
  key_tag: number;
}

export interface DockerSecurity {
  apparmor: boolean;
  cgroup_v2: boolean;
  rootless?: boolean;
  seccomp?: string;
  selinux?: boolean;
  userns: boolean;
}

export interface EnrollmentView {
  command: string;
  expires_at: string;
  install_command: string;
  token: string;
}

export interface Error {
  error: string;
}

export interface FailoverRequest {
  after_s?: number;
  standby_id?: number;
}

export interface FailoverView {
  after_s: number;
  standby_id: number;
  witnesses: WitnessView[];
}

export interface FileOpRequest {
  content?: string;
  mode?: string;
  overwrite?: boolean;
  path?: string;
  paths?: string[];
  to?: string;
}

export interface FilesEntry {
  mod_time: string;
  mode: string;
  name: string;
  size: number;
  target?: string;
  type: string;
}

export interface FilesExtractResult {
  bytes: number;
  dirs: number;
  files: number;
  skipped?: string[];
}

export interface ForgeSourceRequest {
  allow_private?: boolean;
  kind?: string;
  name?: string;
  token?: string;
  url?: string;
}

export interface ForwarderRequest {
  local?: string;
  recipients?: string[];
}

export interface GitSourceView {
  base_url?: string;
  created_at: string;
  html_url: string;
  id: number;
  install_url: string;
  kind: string;
  name: string;
  owner: string;
  slug: string;
}

export interface GitView {
  branch?: string;
  build_env?: string[];
  builder?: string;
  compose_file?: string;
  context?: string;
  dockerfile?: string;
  repo?: string;
  source_id?: number;
  url?: string;
}

export interface GitViewOut {
  branch: string;
  build_env?: string[];
  builder?: string;
  compose_file?: string;
  context: string;
  dockerfile: string;
  repo?: string;
  source_id?: number;
  url: string;
}

export interface HealthCheckRequest {
  host?: string;
  host_header?: string;
  insecure?: boolean;
  interval?: number;
  kind?: string;
  name?: string;
  path?: string;
  port?: number;
  project_id?: number;
  timeout?: number;
}

export interface HealthCheckView {
  host: string;
  host_header: string;
  id: number;
  insecure: boolean;
  interval: number;
  kind: string;
  name: string;
  path: string;
  port: number;
  project_id: number;
  results: HealthResultView[];
  status: string;
  status_since: string | null;
  timeout: number;
}

export interface HealthEventView {
  at: string;
  detail: string;
  status: string;
}

export interface HealthResultView {
  at: string;
  error?: string;
  latency_ms: number;
  ok: boolean;
  server_id: number;
  server_name: string;
  stale: boolean;
}

export interface ImageChange {
  from?: string;
  service: string;
  to?: string;
}

export interface InstallRequest {
  inputs?: Record<string, string>;
  name?: string;
  project_id?: number;
  server_id?: number;
}

export interface InstallationView {
  account: string;
  all: boolean;
  html_url: string;
  id: number;
  pull_requests: boolean;
  statuses: boolean;
}

export interface LinkRequest {
  app_id?: number;
  env_var?: string;
}

export interface LoginRequest {
  email?: string;
  password?: string;
}

export interface MailChecks {
  at: string;
  error?: string;
  results: AgentprotoMailCheckResult[];
}

export interface MailDomainRequest {
  name?: string;
}

export interface MailDomainUpdate {
  catch_all?: string;
}

export interface MailDomainView {
  catch_all: string;
  created_at: string;
  dns_records: DnsRecordOut[];
  id: number;
  managed_dns: boolean;
  name: string;
}

export interface MailRelayRequest {
  host?: string;
  insecure?: boolean;
  password?: string;
  port?: number;
  tls?: string;
  username?: string;
}

export interface MailRelayView {
  has_password: boolean;
  host: string;
  insecure: boolean;
  port: number;
  tls: string;
  username: string;
}

export interface MailSettingsRequest {
  relay?: MailRelayRequest | null;
  send_limit?: number;
}

export interface MailTLSStatus {
  error?: string;
  not_after?: string;
}

export interface MailboxRequest {
  aliases?: string[];
  local?: string;
  name?: string;
  password?: string;
  quota_mb?: number;
}

export interface MailserverForwarder {
  address: string;
  domain_id: string;
  id: string;
  recipients: string[];
}

export interface MailserverMailbox {
  address: string;
  aliases: string[];
  domain_id: string;
  id: string;
  name: string;
  quota_mb: number;
  used_mb: number;
}

export interface ManifestCompleteRequest {
  code?: string;
  state?: string;
}

export interface ManifestRequest {
  name?: string;
  organization?: string;
}

export interface MetricsView {
  at: string;
  cpu_pct: number;
  disk_total: number;
  disk_used: number;
  load1: number;
  load15: number;
  load5: number;
  mem_total: number;
  mem_used: number;
  uptime: number;
}

export interface MoveRequest {
  project_id?: number;
}

export interface NameserversRequest {
  nameservers?: string[];
}

export interface NotificationEvent {
  id: string;
  label: string;
  project: boolean;
}

export interface PanelBackupView {
  bytes: number;
  created_at: string;
  error?: string;
  finished_at?: string;
  id: number;
  log?: string;
  snapshot?: string;
  status: string;
  storage_id?: number;
  trigger: string;
}

export interface PanelURLRequest {
  url?: string;
}

export interface PanelView {
  epoch: number;
  superseded: SupersededView | null;
}

export interface PasskeyLoginRequest {
  authenticator_data?: string;
  challenge_id?: string;
  client_data_json?: string;
  credential_id?: string;
  signature?: string;
  user_handle?: string;
}

export interface PasskeyView {
  created_at: string;
  host: string;
  id: number;
  last_used_at: string | null;
  name: string;
  synced: boolean;
}

export interface PasswordRequest {
  password?: string;
}

export interface PhpLaravel {
  scheduler?: boolean;
  workers?: number;
}

export interface PhpLaravelLogLine {
  error?: boolean;
  message: string;
  process: string;
  time: string;
}

export interface PhpLaravelOut {
  scheduler: boolean;
  workers: number;
}

export interface PhpLaravelProcess {
  name: string;
  restarts: number;
  since?: string;
  state: string;
  sub_state: string;
}

export interface PhpSettings {
  cpu_percent?: number;
  display_errors?: boolean;
  max_children?: number;
  max_execution_time?: number;
  max_input_vars?: number;
  memory_mb?: number;
  ram_mb?: number;
  timezone?: string;
  upload_mb?: number;
}

export interface PhpSettingsOut {
  cpu_percent?: number;
  display_errors?: boolean;
  max_children?: number;
  max_execution_time?: number;
  max_input_vars?: number;
  memory_mb?: number;
  ram_mb?: number;
  timezone?: string;
  upload_mb?: number;
}

export interface PlacementView {
  health_check?: number;
  primary: boolean;
  server_id: number;
  services?: ServiceCheckView[];
  status?: string;
}

export interface PolicyRequest {
  at_minute?: number;
  keep_daily?: number;
  keep_last?: number;
  keep_monthly?: number;
  keep_weekly?: number;
  schedule?: string;
  storage_id?: number;
}

export interface PolicyView {
  at_minute: number;
  keep_daily: number;
  keep_last: number;
  keep_monthly: number;
  keep_weekly: number;
  next_run?: string;
  schedule: string;
  storage_id: number;
}

export interface PreviewsRequest {
  domain?: string;
}

export interface ProjectMemberRequest {
  role?: string;
}

export interface ProjectMemberView {
  created_at: string;
  email: string;
  name: string;
  role: string;
  user_id: number;
}

export interface ProjectRequest {
  description?: string;
  name?: string;
}

export interface ProjectUsageView {
  apps: number;
  cpus: number;
  databases: number;
  disk_bytes: number;
  dns_zones: number;
  health_checks: number;
  memory_mb: number;
  sites: number;
}

export interface ProjectView {
  created_at: string;
  default: boolean;
  description: string;
  id: number;
  limits: StoreProjectLimitsOut;
  members: number;
  name: string;
  resources: number;
  role: string;
}

export interface ProxySyncView {
  applied_revision: number;
  error?: string;
  in_sync: boolean;
  revision: number;
}

export interface PublicAccessRequest {
  allow?: string[];
  port?: number;
  require_tls?: boolean;
}

export interface PublicAccessView {
  allow: string[];
  ca_cert: string;
  host: string;
  port: number;
  require_tls: boolean;
  url: string;
}

export interface PublicIPsRequest {
  ips?: string[];
}

export interface RecoveryRequest {
  passphrase?: string;
  password?: string;
}

export interface RegistryRequest {
  host?: string;
  password?: string;
  username?: string;
}

export interface RegistryView {
  created_at: string;
  host: string;
  id: number;
  updated_at: string;
  username: string;
}

export interface RenamePasskeyRequest {
  name?: string;
}

export interface ReplicaRequest {
  server_id?: number;
}

export interface RepoView {
  default_branch: string;
  full_name: string;
  html_url: string;
  private: boolean;
}

export interface ResetPasswordRequest {
  password?: string;
  reset_totp?: boolean;
}

export interface RestoreCopyRequest {
  path?: string;
  volume?: string;
}

export interface RestoreRequest {
  snapshots?: string[];
}

export interface RouteView {
  app_id?: number;
  created_at: string;
  dns_challenge?: boolean;
  domains: string[];
  hsts: boolean;
  id: number;
  kind: string;
  root?: string;
  site_id?: number;
  tls: string;
  upstream?: string;
}

export interface ServerView {
  agent_version?: string;
  arch?: string;
  cert_expires?: string;
  cpu_count?: number;
  created_at: string;
  detected_ips: string[];
  distro?: string;
  dns_ips: string[];
  docker: string;
  docker_security?: DockerSecurity;
  enroll_expires?: string;
  hostname?: string;
  id: number;
  isolated: boolean;
  kernel?: string;
  last_ip?: string;
  last_seen?: string;
  mariadb: string;
  mem_total?: number;
  name: string;
  os?: string;
  peer_error?: string;
  peer_port: number;
  php_limits: boolean;
  php_versions: string[];
  public_ips: string[];
  railpack: string;
  restic: string;
  status: string;
}

export interface ServiceCheckView {
  health_check: number;
  service: string;
  status: string;
}

export interface SessionView {
  created_at: string;
  current: boolean;
  expires_at: string;
  id: string;
  ip: string;
  last_seen_at: string;
  user_agent: string;
}

export interface SetupRequest {
  email?: string;
  name?: string;
  password?: string;
  setup_token?: string;
}

export interface SftpConnectionView {
  host_key_fingerprint: string;
  port: number;
}

export interface SftpUserView {
  created_at: string;
  has_password: boolean;
  id: number;
  public_keys: string[];
  username: string;
}

export interface SftpdBan {
  failures?: number;
  ip: string;
  strikes: number;
  until: string;
}

export interface SiteDomainsRequest {
  domains?: string[];
}

export interface SiteInstallRequest {
  admin_email?: string;
  admin_user?: string;
  app?: string;
  title?: string;
}

export interface SitePHPRequest {
  version?: string;
}

export interface SitePlacementView {
  databases?: DbCopyView;
  health_check?: number;
  primary: boolean;
  server_id: number;
  status?: string;
  sync_error?: string;
  synced_at?: string;
  syncing?: boolean;
}

export interface SitePrimaryRequest {
  force?: boolean;
  server_id?: number;
}

export interface SiteQuotaRequest {
  quota_mb?: number;
}

export interface SiteUsageView {
  at: string | null;
  db_bytes: number;
  disk_bytes: number;
  disk_files: number;
}

export interface SiteView {
  created_at: string;
  domains: string[];
  id: number;
  kind: string;
  laravel: PhpLaravelOut;
  name: string;
  over_quota: boolean;
  php_settings: PhpSettingsOut;
  php_version: string;
  project_id: number;
  quota_mb: number;
  replicas: number[];
  server_id: number;
  tls: string;
  usage: SiteUsageView;
}

export interface StandbyEnrollmentView {
  expires_at: string;
  install_command: string;
  token: string;
}

export interface StandbyView {
  agent_url?: string;
  applied_at: string | null;
  cert_expires_at: string | null;
  connected: boolean;
  enrolled: boolean;
  error?: string;
  id: number;
  last_ip: string;
  last_seen_at: string | null;
  name: string;
  ready: boolean;
  server_id?: number;
  version?: string;
}

export interface StartCheckRequest {
  server_id?: number;
  storage_id?: number;
}

export interface StorageRequest {
  access_key?: string;
  bucket?: string;
  endpoint?: string;
  host?: string;
  kind?: string;
  name?: string;
  path?: string;
  port?: number;
  region?: string;
  secret_key?: string;
  user?: string;
}

export interface StorageView {
  access_key?: string;
  bucket?: string;
  created_at: string;
  endpoint?: string;
  host?: string;
  id: number;
  kind: string;
  name: string;
  path: string;
  policies: number;
  port?: number;
  region?: string;
  ssh_public_key?: string;
  user?: string;
}

export interface StoreAppRoute {
  domains?: string[];
  port?: number;
  service?: string;
}

export interface StoreAppRouteOut {
  domains: string[];
  port: number;
  service: string;
}

export interface StoreComposeService {
  build?: string;
  cpus?: number;
  image: string;
  job?: boolean;
  memory_mb?: number;
  name: string;
  ports?: number[];
}

export interface StoreComposeSummary {
  services: StoreComposeService[];
  volumes: string[];
}

export interface StoreProjectLimits {
  apps?: number;
  cpus?: number;
  databases?: number;
  disk_mb?: number;
  dns_zones?: number;
  health_checks?: number;
  memory_mb?: number;
  sites?: number;
}

export interface StoreProjectLimitsOut {
  apps: number;
  cpus: number;
  databases: number;
  disk_mb: number;
  dns_zones: number;
  health_checks: number;
  memory_mb: number;
  sites: number;
}

export interface SupersededView {
  at: string;
  epoch: number;
}

export interface TemplateInfoView {
  id: string;
  name: string;
  notes: string;
  revision: number;
  version: string;
  website: string;
}

export interface TemplateUpdate {
  backup_id?: number;
  error?: string;
  revision: number;
  state: string;
}

export interface TemplateUpdateRequest {
  backup?: boolean;
  inputs?: Record<string, string>;
}

export interface TemplateView {
  category: string;
  compose: string;
  description: string;
  expose: CatalogExpose;
  id: string;
  inputs: CatalogInput[];
  mail?: boolean;
  memory_mb: number;
  name: string;
  notes: string;
  revision: number;
  secrets: CatalogSecret[];
  services: CatalogService[];
  source?: string;
  version: string;
  volumes: string[];
  website: string;
}

export interface TokenOwner {
  email: string;
  id: number;
}

export interface TokenView {
  created_at: string;
  expired: boolean;
  expires_at: string | null;
  id: number;
  last_used_at: string | null;
  last_used_ip?: string;
  name: string;
  prefix: string;
  project_id?: number;
  role: string;
  user?: TokenOwner;
}

export interface UpdateRun {
  by: string;
  error?: string;
  finished_at?: string;
  servers: UpdateRunServer[];
  started_at: string;
  state: string;
  version: string;
}

export interface UpdateRunServer {
  error?: string;
  id: number;
  name: string;
  status: string;
}

export interface UpdateSFTPUserRequest {
  password?: string;
  public_keys?: string[];
}

export interface UpdateServerView {
  arch: string;
  id: number;
  name: string;
  online: boolean;
  updatable: boolean;
  version: string;
}

export interface UpdateUserRequest {
  disabled?: boolean;
  name?: string;
  role?: string;
}

export interface UserView {
  created_at: string;
  disabled: boolean;
  email: string;
  id: number;
  name: string;
  passkeys?: number;
  role: string;
  totp_enabled: boolean;
}

export interface WebhookRequest {
  auto_deploy?: boolean;
  regenerate?: boolean;
}

export interface WebhookView {
  auto_deploy: boolean;
  path: string;
  secret: string;
}

export interface WitnessView {
  addr: string;
  name: string;
  server_id: number;
}

export interface WordpressAdmin {
  email: string;
  login_url: string;
  password: string;
  user: string;
}

// Responses and bodies of the routes.

export type DeleteAccountPasskeysIdResponse = void;

export type DeleteAccountSessionsResponse = void;

export type DeleteAccountSessionsIdResponse = void;

export type DeleteAccountTokensIdResponse = void;

export type DeleteAppsIdResponse = {
  cleanup_error?: string;
  sync: ProxySyncView;
};

export type DeleteAppsIdBackupPolicyResponse = {
  ok: boolean;
};

export type DeleteAppsIdLinksAppResponse = {
  app: AppView;
  attach_error?: string;
};

export type DeleteAppsIdReplicasSidResponse = {
  app?: AppView;
  cleanup_error?: string;
};

export type DeleteBackupStoragesIdResponse = {
  ok: boolean;
};

export type DeleteDnsZonesIdResponse = void;

export type DeleteDnsZonesIdRecordsRidResponse = void;

export type DeleteGitSourcesIdResponse = void;

export type DeleteHealthChecksIdResponse = void;

export type DeleteMailDomainsIdResponse = void;

export type DeleteMailDomainsIdForwardersFidResponse = void;

export type DeleteMailDomainsIdMailboxesMidResponse = void;

export type DeleteNotificationsIdResponse = void;

export type DeletePanelBackupPolicyResponse = {
  ok: boolean;
};

export type DeleteProjectsIdResponse = void;

export type DeleteProjectsIdMembersUidResponse = void;

export type DeleteRegistriesIdResponse = void;

export type DeleteServersIdResponse = void;

export type DeleteServersIdRoutesRidResponse = {
  sync: ProxySyncView;
};

export type DeleteServersIdSftpBansIpResponse = {
  bans: SftpdBan[];
};

export type DeleteServersIdTrashNameResponse = {
  entries: AgentprotoTrashEntry[];
  keep_days: number;
};

export type DeleteSitesIdResponse = {
  cleanup_error?: string;
  sync: ProxySyncView;
};

export type DeleteSitesIdAdminerResponse = {
  adminer: AdminerView;
};

export type DeleteSitesIdBackupPolicyResponse = {
  ok: boolean;
};

export type DeleteSitesIdDatabasesDidResponse = void;

export type DeleteSitesIdReplicasSidResponse = {
  cleanup_error?: string;
  placements: SitePlacementView[];
  site: SiteView;
};

export type DeleteSitesIdSftpUsersUidResponse = {
  ok: boolean;
  sync_error?: string;
};

export type DeleteStandbysIdResponse = void;

export type DeleteTokensIdResponse = void;

export type DeleteUsersIdResponse = void;

export type GetAccountPasskeysResponse = {
  passkeys: PasskeyView[];
};

export type GetAccountSessionsResponse = {
  sessions: SessionView[];
};

export type GetAccountTokensResponse = {
  tokens: TokenView[];
};

export type GetAppsResponse = {
  apps: AppView[];
};

export type GetAppsIdResponse = {
  app: AppView;
  placements: PlacementView[];
  sync: ProxySyncView;
};

export type GetAppsIdAccessLogResponse = {
  entries: AgentprotoAccessEntry[];
};

export type GetAppsIdBackupsResponse = {
  backups: BackupView[];
  items: AgentprotoBackupItem[];
  policy: PolicyView | null;
  restic: string;
};

export type GetAppsIdBackupsBidResponse = {
  backup: BackupView;
};

export type GetAppsIdComposeResponse = {
  compose: string;
  error?: string;
  port: number;
  service: string;
  warnings: string[];
};

export type GetAppsIdConnectionResponse = {
  connection: ConnectionView;
};

export type GetAppsIdDeploymentsResponse = {
  deployments: DeploymentView[];
};

export type GetAppsIdDeploymentsDidResponse = {
  deployment: DeploymentView;
};

export type GetAppsIdEnvResponse = {
  env: Record<string, string>;
};

export type GetAppsIdLogsResponse = {
  log: string;
};

export type GetAppsIdMetricsResponse = {
  metrics: AppMetricsView[];
};

export type GetAppsIdPreviewsResponse = {
  domain: string;
  max: number;
  previews: AppView[];
  supported: boolean;
};

export type GetAppsIdSnapshotsResponse = {
  snapshots: AgentprotoSnapshotInfo[];
};

export type GetAppsIdSnapshotsSidFilesResponse = {
  entries: AgentprotoSnapshotEntry[];
  path: string;
};

export type GetAppsIdStatusResponse = {
  status: AgentprotoAppStatus;
};

export type GetAppsIdTemplateResponse = {
  backups: boolean;
  images?: ImageChange[];
  installed_revision: number;
  needs?: CatalogInput[];
  template: TemplateInfoView | null;
  update: TemplateUpdate | null;
  update_available: boolean;
};

export type GetAppsIdUpgradeResponse = DbUpgradeView;

export type GetAppsIdWebhookResponse = {
  webhook: WebhookView;
};

export type GetAuditResponse = {
  actions: string[];
  entries: AuditView[];
  more: boolean;
};

export type GetAuditRetentionResponse = AuditRetentionView;

export type GetAuditVerifyResponse = AuditCheckView;

export type GetAuthMeResponse = {
  user: UserView;
};

export type GetBackupChecksResponse = {
  repositories: CheckRepoView[];
};

export type GetBackupChecksCidResponse = {
  check: CheckView;
};

export type GetBackupStoragesResponse = {
  storages: StorageView[];
};

export type GetBackupStoragesIdPasswordResponse = {
  password: string;
};

export type GetCatalogResponse = {
  source: CatalogSourceView | null;
  templates: TemplateView[];
};

export type GetDatabaseEnginesResponse = {
  engines: DbengineEngine[];
};

export type GetDnsResponse = {
  nameservers: string[];
  servers: DnsServerView[];
  zones: DnsZoneView[];
};

export type GetDnsZonesIdResponse = {
  auto: DnsAutoRecord[];
  records: DnsRecordView[];
  types: string[];
  zone: DnsZoneView;
};

export type GetGitSourcesResponse = {
  panel_url: string;
  sources: GitSourceView[];
};

export type GetGitSourcesIdResponse = {
  error?: string;
  installations: InstallationView[];
  source: GitSourceView;
};

export type GetGitSourcesIdBranchesResponse = {
  branches: string[];
};

export type GetGitSourcesIdReposResponse = {
  repos: RepoView[];
};

export type GetHealthChecksResponse = {
  checks: HealthCheckView[];
};

export type GetHealthChecksIdEventsResponse = {
  events: HealthEventView[];
};

export type GetMailResponse = {
  checks?: MailChecks | null;
  domains: MailDomainView[];
  hostname?: string;
  ports: number[];
  server: AppView | null;
  settings?: {
    relay: MailRelayView | null;
    send_limit: number;
  };
  tls?: MailTLSStatus;
};

export type GetMailDomainsIdForwardersResponse = {
  forwarders: MailserverForwarder[];
};

export type GetMailDomainsIdMailboxesResponse = {
  mailboxes: MailserverMailbox[];
};

export type GetNotificationsResponse = {
  channels: ChannelView[];
  events: NotificationEvent[];
  panel_url: string;
};

export type GetOpenapiJsonResponse = Record<string, unknown>;

export type GetPanelResponse = {
  panel: PanelView;
};

export type GetPanelBackupResponse = {
  backups: PanelBackupView[];
  policy: PolicyView | null;
  recovery: {
    set: boolean;
    set_at?: string;
  };
  restic: string;
  restic_error?: string;
};

export type GetPanelBackupRunsBidResponse = {
  backup: PanelBackupView;
};

export type GetProjectsResponse = {
  projects: ProjectView[];
};

export type GetProjectsIdResponse = {
  members: ProjectMemberView[];
  project: ProjectView;
  usage: ProjectUsageView;
};

export type GetProjectsIdAuditResponse = {
  actions: string[];
  entries: AuditView[];
  more: boolean;
};

export type GetRegistriesResponse = {
  registries: RegistryView[];
};

export type GetServersResponse = {
  servers: ServerView[];
};

export type GetServersIdResponse = {
  metrics?: MetricsView;
  server: ServerView;
};

export type GetServersIdCertificatesResponse = {
  certificates: CertView[];
};

export type GetServersIdMetricsResponse = {
  metrics: MetricsView[];
};

export type GetServersIdRoutesResponse = {
  routes: RouteView[];
  sync: ProxySyncView;
};

export type GetServersIdSftpBansResponse = {
  bans: SftpdBan[];
};

export type GetServersIdTrashResponse = {
  entries: AgentprotoTrashEntry[];
  keep_days: number;
};

export type GetSetupResponse = {
  required: boolean;
};

export type GetSitesResponse = {
  sites: SiteView[];
};

export type GetSitesIdResponse = {
  placements: SitePlacementView[];
  site: SiteView;
  sync: ProxySyncView;
};

export type GetSitesIdAccessLogResponse = {
  entries: AgentprotoAccessEntry[];
};

export type GetSitesIdAdminerResponse = {
  adminer: AdminerView;
};

export type GetSitesIdBackupsResponse = {
  backups: BackupView[];
  items: AgentprotoBackupItem[];
  policy: PolicyView | null;
  restic: string;
};

export type GetSitesIdBackupsBidResponse = {
  backup: BackupView;
};

export type GetSitesIdDatabasesResponse = {
  databases: DatabaseView[];
  mariadb: string;
};

export type GetSitesIdFilesResponse = {
  entries: FilesEntry[];
  path: string;
};

export type GetSitesIdFilesStatResponse = {
  entry: FilesEntry | null;
};

export type GetSitesIdFilesTextResponse = {
  content: string;
  size: number;
};

export type GetSitesIdLaravelResponse = {
  processes: PhpLaravelProcess[];
};

export type GetSitesIdLaravelLogResponse = {
  lines: PhpLaravelLogLine[];
};

export type GetSitesIdPhpLogResponse = {
  log: string;
  path: string;
  size: number;
  truncated?: boolean;
};

export type GetSitesIdSftpUsersResponse = {
  connection: SftpConnectionView | null;
  users: SftpUserView[];
};

export type GetSitesIdSnapshotsResponse = {
  snapshots: AgentprotoSnapshotInfo[];
};

export type GetSitesIdSnapshotsSidFilesResponse = {
  entries: AgentprotoSnapshotEntry[];
  path: string;
};

export type GetStandbysResponse = {
  available: boolean;
  failover: FailoverView;
  standbys: StandbyView[];
};

export type GetTokensResponse = {
  tokens: TokenView[];
};

export type GetUpdatesResponse = {
  available: boolean;
  checked_at: string;
  current: string;
  enabled: boolean;
  error: string;
  latest: string;
  run: UpdateRun | null;
  running: boolean;
  servers: UpdateServerView[];
};

export type GetUsersResponse = {
  users: UserView[];
};

export type GetVersionResponse = {
  commit: string;
  version: string;
};

export type PostAccountPasskeysResponse = {
  passkey: PasskeyView;
};

export type PostAccountPasskeysBody = AddPasskeyRequest;

export type PostAccountPasskeysOptionsResponse = {
  options: Record<string, unknown>;
};

export type PostAccountPasskeysOptionsBody = PasswordRequest;

export type PostAccountTokensResponse = {
  secret: string;
  token: TokenView;
};

export type PostAccountTokensBody = CreateTokenRequest;

export type PostAppsResponse = {
  app: AppView;
  sync: ProxySyncView;
  warnings: string[];
};

export type PostAppsBody = CreateAppRequest;

export type PostAppsIdBackupsResponse = {
  backup: BackupView;
};

export type PostAppsIdComposeLoadResponse = {
  app: AppView;
  commit: string;
  warnings: string[];
};

export type PostAppsIdDeployResponse = {
  deployment: DeploymentView;
};

export type PostAppsIdDeployBody = DeployRequest;

export type PostAppsIdDeployKeyResponse = {
  app: AppView;
};

export type PostAppsIdDeploymentsDidCancelResponse = Record<string, never>;

export type PostAppsIdLinksResponse = {
  app: AppView;
  attach_error?: string;
};

export type PostAppsIdLinksBody = LinkRequest;

export type PostAppsIdPasswordResponse = {
  connection: ConnectionView;
  deploy_error?: string;
  redeploying: string[];
  updated_apps: string[];
};

export type PostAppsIdPasswordBody = DbPasswordRequest;

export type PostAppsIdReplicasResponse = {
  app?: AppView;
  deploy_error?: string;
  deploying: boolean;
};

export type PostAppsIdReplicasBody = ReplicaRequest;

export type PostAppsIdRestoreResponse = {
  backup: BackupView;
};

export type PostAppsIdRestoreBody = RestoreRequest;

export type PostAppsIdSnapshotsSidRestoreCopyResponse = AgentprotoRestoreCopyResponse;

export type PostAppsIdSnapshotsSidRestoreCopyBody = RestoreCopyRequest;

export type PostAppsIdTemplateUpdateResponse = {
  update: TemplateUpdate;
};

export type PostAppsIdTemplateUpdateBody = TemplateUpdateRequest;

export type PostAppsIdUpgradeResponse = DbUpgradeStarted;

export type PostAppsIdUpgradeBody = DbUpgradeRequest;

export type PostAuditAnchorsResetResponse = {
  anchors: AuditAnchorView[];
};

export type PostAuthLoginResponse = {
  mfa_required: boolean;
  user?: UserView;
};

export type PostAuthLoginBody = LoginRequest;

export type PostAuthLogoutResponse = void;

export type PostAuthMfaResponse = {
  recovery_codes_remaining?: number;
  user: UserView;
};

export type PostAuthMfaBody = CodeRequest;

export type PostAuthPasskeyResponse = {
  user: UserView;
};

export type PostAuthPasskeyBody = PasskeyLoginRequest;

export type PostAuthPasskeyOptionsResponse = {
  challenge_id: string;
  options: Record<string, unknown>;
};

export type PostAuthPasswordResponse = void;

export type PostAuthPasswordBody = ChangePasswordRequest;

export type PostAuthTotpDisableResponse = void;

export type PostAuthTotpDisableBody = DisableTOTPRequest;

export type PostAuthTotpEnableResponse = {
  recovery_codes: string[];
};

export type PostAuthTotpEnableBody = CodeRequest;

export type PostAuthTotpSetupResponse = {
  secret: string;
  uri: string;
};

export type PostAuthTotpSetupBody = PasswordRequest;

export type PostBackupChecksResponse = {
  check: CheckView;
};

export type PostBackupChecksBody = StartCheckRequest;

export type PostBackupStoragesResponse = {
  storage: StorageView;
};

export type PostBackupStoragesBody = StorageRequest;

export type PostBackupStoragesIdCheckResponse = {
  error?: string;
  ok: boolean;
};

export type PostBackupStoragesIdCheckBody = CheckStorageRequest;

export type PostCatalogIdInstallResponse = {
  app: AppView;
  attach_error?: string;
  deploy_error?: string;
  deployment?: DeploymentView;
  notes: string;
};

export type PostCatalogIdInstallBody = InstallRequest;

export type PostCatalogRefreshResponse = {
  source: CatalogSourceView | null;
};

export type PostDnsZonesResponse = {
  zone: DnsZoneView;
};

export type PostDnsZonesBody = CreateZoneRequest;

export type PostDnsZonesIdRecordsResponse = {
  record: DnsRecordView;
};

export type PostDnsZonesIdRecordsBody = DnsRecord;

export type PostGitSourcesResponse = {
  source: GitSourceView;
};

export type PostGitSourcesBody = ForgeSourceRequest;

export type PostGitSourcesGithubCompleteResponse = {
  source: GitSourceView;
};

export type PostGitSourcesGithubCompleteBody = ManifestCompleteRequest;

export type PostGitSourcesGithubManifestResponse = {
  action: string;
  manifest: string;
  state: string;
};

export type PostGitSourcesGithubManifestBody = ManifestRequest;

export type PostHealthChecksResponse = {
  check: {
    id: number;
    name: string;
  };
};

export type PostHealthChecksBody = HealthCheckRequest;

export type PostMailResponse = {
  deploy_error?: string;
  deployment?: DeploymentView;
  server: AppView;
};

export type PostMailBody = CreateMailRequest;

export type PostMailChecksResponse = {
  checks: MailChecks;
};

export type PostMailDomainsResponse = {
  domain: MailDomainView;
};

export type PostMailDomainsBody = MailDomainRequest;

export type PostMailDomainsIdForwardersResponse = {
  forwarder: MailserverForwarder;
};

export type PostMailDomainsIdForwardersBody = ForwarderRequest;

export type PostMailDomainsIdMailboxesResponse = {
  mailbox: {
    address: string;
    id: string;
  };
};

export type PostMailDomainsIdMailboxesBody = MailboxRequest;

export type PostNotificationsResponse = {
  channel: ChannelView;
};

export type PostNotificationsBody = ChannelRequest;

export type PostNotificationsIdTestResponse = {
  error?: string;
  ok: boolean;
};

export type PostPanelBackupRunResponse = {
  backup: PanelBackupView;
};

export type PostProjectsResponse = {
  project: ProjectView;
};

export type PostProjectsBody = ProjectRequest;

export type PostProjectsIdMembersResponse = {
  member: ProjectMemberView;
};

export type PostProjectsIdMembersBody = AddMemberRequest;

export type PostRegistriesResponse = {
  registry: RegistryView;
};

export type PostRegistriesBody = RegistryRequest;

export type PostServersResponse = {
  enrollment: EnrollmentView;
  server: ServerView;
};

export type PostServersBody = CreateServerRequest;

export type PostServersIdEnrollTokenResponse = {
  enrollment: EnrollmentView;
};

export type PostServersIdPingResponse = {
  agent_time: string;
  rtt_ms: number;
};

export type PostServersIdRoutesResponse = {
  route: RouteView;
  sync: ProxySyncView;
};

export type PostServersIdRoutesBody = CreateRouteRequest;

export type PostServersIdTrashNameRestoreResponse = {
  provision_error?: string;
  site: SiteView;
};

export type PostServersIdTrashNameRestoreBody = CreateSiteRequest;

export type PostSetupResponse = {
  user: UserView;
};

export type PostSetupBody = SetupRequest;

export type PostSitesResponse = {
  provision_error?: string;
  site: SiteView;
  sync: ProxySyncView;
};

export type PostSitesBody = CreateSiteRequest;

export type PostSitesIdAdminerResponse = {
  adminer: AdminerView;
};

export type PostSitesIdBackupsResponse = {
  backup: BackupView;
};

export type PostSitesIdDatabasesResponse = {
  credentials: DbCredentialsView;
  database: DatabaseView;
};

export type PostSitesIdDatabasesBody = CreateDatabaseRequest;

export type PostSitesIdDatabasesDidPasswordResponse = {
  credentials: DbCredentialsView;
};

export type PostSitesIdFilesOpResponse = {
  extract?: FilesExtractResult;
  ok: boolean;
};

export type PostSitesIdFilesOpBody = FileOpRequest;

export type PostSitesIdInstallResponse = {
  admin?: WordpressAdmin;
  app: string;
  database: DatabaseView;
  files: number;
  url?: string;
  version: string;
};

export type PostSitesIdInstallBody = SiteInstallRequest;

export type PostSitesIdLaravelRestartResponse = {
  processes: PhpLaravelProcess[];
};

export type PostSitesIdReplicasResponse = {
  placements: SitePlacementView[];
  site: SiteView;
};

export type PostSitesIdReplicasBody = ReplicaRequest;

export type PostSitesIdRestoreResponse = {
  backup: BackupView;
};

export type PostSitesIdRestoreBody = RestoreRequest;

export type PostSitesIdSftpUsersResponse = {
  connection: SftpConnectionView | null;
  password?: string;
  sync_error?: string;
  user: SftpUserView;
};

export type PostSitesIdSftpUsersBody = CreateSFTPUserRequest;

export type PostSitesIdSnapshotsSidRestoreCopyResponse = AgentprotoRestoreCopyResponse;

export type PostSitesIdSnapshotsSidRestoreCopyBody = RestoreCopyRequest;

export type PostSitesIdSyncResponse = {
  placements: SitePlacementView[];
  site: SiteView;
};

export type PostSitesIdUsageResponse = {
  site: SiteView;
};

export type PostStandbysResponse = {
  enrollment: StandbyEnrollmentView;
  server: ServerView;
  standby: StandbyView;
};

export type PostStandbysBody = CreateStandbyRequest;

export type PostUpdatesApplyResponse = {
  run: UpdateRun;
};

export type PostUpdatesCheckResponse = {
  available: boolean;
  checked_at: string;
  current: string;
  enabled: boolean;
  error: string;
  latest: string;
  run: UpdateRun | null;
  running: boolean;
  servers: UpdateServerView[];
};

export type PostUsersResponse = {
  user: UserView;
};

export type PostUsersBody = CreateUserRequest;

export type PostWebhooksAppsIdResponse = {
  commit?: string;
  result: string;
};

export type PostWebhooksGithubResponse = {
  deploying?: string[];
  result: string;
};

export type PutAccountPasskeysIdResponse = void;

export type PutAccountPasskeysIdBody = RenamePasskeyRequest;

export type PutAppsIdResponse = {
  app: AppView;
};

export type PutAppsIdBody = AppSettingsRequest;

export type PutAppsIdBackupPolicyResponse = {
  policy: PolicyView;
};

export type PutAppsIdBackupPolicyBody = PolicyRequest;

export type PutAppsIdComposeResponse = {
  app: AppView;
  warnings: string[];
};

export type PutAppsIdComposeBody = ComposeRequest;

export type PutAppsIdDomainsResponse = {
  app: AppView;
  sync: ProxySyncView;
};

export type PutAppsIdDomainsBody = AppDomainsRequest;

export type PutAppsIdEnvResponse = {
  ok: boolean;
};

export type PutAppsIdEnvBody = AppEnvRequest;

export type PutAppsIdPreviewsResponse = {
  domain: string;
  max: number;
  previews: AppView[];
  supported: boolean;
};

export type PutAppsIdPreviewsBody = PreviewsRequest;

export type PutAppsIdProjectResponse = void;

export type PutAppsIdProjectBody = MoveRequest;

export type PutAppsIdPublicAccessResponse = {
  apply_error?: string;
  connection: ConnectionView;
};

export type PutAppsIdPublicAccessBody = PublicAccessRequest;

export type PutAppsIdRoutesResponse = {
  app: AppView;
  sync: ProxySyncView;
};

export type PutAppsIdRoutesBody = AppRoutesRequest;

export type PutAppsIdWebhookResponse = {
  webhook: WebhookView;
};

export type PutAppsIdWebhookBody = WebhookRequest;

export type PutAuditRetentionResponse = AuditRetentionView;

export type PutAuditRetentionBody = AuditRetentionRequest;

export type PutBackupStoragesIdResponse = {
  storage: StorageView;
};

export type PutBackupStoragesIdBody = StorageRequest;

export type PutCatalogSourceResponse = {
  source: CatalogSourceView | null;
};

export type PutCatalogSourceBody = CatalogSourceRequest;

export type PutDnsNameserversResponse = {
  nameservers: string[];
};

export type PutDnsNameserversBody = NameserversRequest;

export type PutDnsZonesIdDnssecResponse = {
  zone: DnsZoneView;
};

export type PutDnsZonesIdDnssecBody = DnssecRequest;

export type PutDnsZonesIdProjectResponse = void;

export type PutDnsZonesIdProjectBody = MoveRequest;

export type PutDnsZonesIdRecordsRidResponse = {
  record: DnsRecordView;
};

export type PutDnsZonesIdRecordsRidBody = DnsRecord;

export type PutHealthChecksIdResponse = void;

export type PutHealthChecksIdBody = HealthCheckRequest;

export type PutHealthChecksIdProjectResponse = void;

export type PutHealthChecksIdProjectBody = MoveRequest;

export type PutMailDomainsIdResponse = void;

export type PutMailDomainsIdBody = MailDomainUpdate;

export type PutMailDomainsIdForwardersFidResponse = void;

export type PutMailDomainsIdForwardersFidBody = ForwarderRequest;

export type PutMailDomainsIdMailboxesMidResponse = void;

export type PutMailDomainsIdMailboxesMidBody = MailboxRequest;

export type PutMailSettingsResponse = {
  settings: {
    relay: MailRelayView | null;
    send_limit: number;
  };
  warning?: string;
};

export type PutMailSettingsBody = MailSettingsRequest;

export type PutNotificationsIdResponse = {
  channel: ChannelView;
};

export type PutNotificationsIdBody = ChannelRequest;

export type PutNotificationsPanelUrlResponse = {
  panel_url: string;
};

export type PutNotificationsPanelUrlBody = PanelURLRequest;

export type PutPanelBackupPolicyResponse = {
  policy: PolicyView;
};

export type PutPanelBackupPolicyBody = PolicyRequest;

export type PutPanelBackupRecoveryResponse = {
  ok: boolean;
};

export type PutPanelBackupRecoveryBody = RecoveryRequest;

export type PutProjectsIdResponse = {
  project: ProjectView;
};

export type PutProjectsIdBody = ProjectRequest;

export type PutProjectsIdLimitsResponse = {
  project: ProjectView;
  usage: ProjectUsageView;
};

export type PutProjectsIdLimitsBody = StoreProjectLimits;

export type PutProjectsIdMembersUidResponse = void;

export type PutProjectsIdMembersUidBody = ProjectMemberRequest;

export type PutRegistriesIdResponse = {
  registry: RegistryView;
};

export type PutRegistriesIdBody = RegistryRequest;

export type PutServersIdPublicIpsResponse = {
  server: ServerView;
};

export type PutServersIdPublicIpsBody = PublicIPsRequest;

export type PutSitesIdBackupPolicyResponse = {
  policy: PolicyView;
};

export type PutSitesIdBackupPolicyBody = PolicyRequest;

export type PutSitesIdDomainsResponse = {
  site: SiteView;
  sync: ProxySyncView;
};

export type PutSitesIdDomainsBody = SiteDomainsRequest;

export type PutSitesIdFilesContentResponse = {
  path: string;
  size: number;
};

export type PutSitesIdLaravelResponse = {
  site: SiteView;
};

export type PutSitesIdLaravelBody = PhpLaravel;

export type PutSitesIdPhpResponse = {
  site: SiteView;
  sync: ProxySyncView;
};

export type PutSitesIdPhpBody = SitePHPRequest;

export type PutSitesIdPhpSettingsResponse = {
  site: SiteView;
};

export type PutSitesIdPhpSettingsBody = PhpSettings;

export type PutSitesIdPrimaryResponse = {
  placements: SitePlacementView[];
  site: SiteView;
  warning?: string;
};

export type PutSitesIdPrimaryBody = SitePrimaryRequest;

export type PutSitesIdProjectResponse = void;

export type PutSitesIdProjectBody = MoveRequest;

export type PutSitesIdQuotaResponse = {
  site: SiteView;
};

export type PutSitesIdQuotaBody = SiteQuotaRequest;

export type PutSitesIdSftpUsersUidResponse = {
  password?: string;
  sync_error?: string;
  user: SftpUserView;
};

export type PutSitesIdSftpUsersUidBody = UpdateSFTPUserRequest;

export type PutStandbysFailoverResponse = {
  failover: FailoverView;
};

export type PutStandbysFailoverBody = FailoverRequest;

export type PutUsersIdResponse = {
  user: UserView;
};

export type PutUsersIdBody = UpdateUserRequest;

export type PutUsersIdPasswordResponse = void;

export type PutUsersIdPasswordBody = ResetPasswordRequest;

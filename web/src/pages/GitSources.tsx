import { useEffect, useRef, useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate, useSearchParams } from "react-router";
import { api, can, type GitSource, type User } from "../api";
import { ErrorBox, Field, Icon, Loading, Modal, errorMessage, formatDate, useConfirm, useToast } from "../components/ui";

// GitSourcesPage lists the GitHub Apps the panel created to reach the
// user's repositories, and connects new ones.
export function GitSourcesPage({ user }: { user: User }) {
  const sources = useQuery({ queryKey: ["git-sources"], queryFn: api.gitSources, enabled: can(user, "admin") });
  const [connecting, setConnecting] = useState<"github" | "gitlab" | "gitea" | null>(null);
  if (!can(user, "admin")) return <div className="empty">You do not have access to this page.</div>;
  const buttons = (
    <div className="row wrap">
      <button className="btn primary" onClick={() => setConnecting("github")}>
        <Icon name="plus" /> GitHub
      </button>
      <button className="btn" onClick={() => setConnecting("gitlab")}>
        <Icon name="plus" /> GitLab
      </button>
      <button className="btn" onClick={() => setConnecting("gitea")}>
        <Icon name="plus" /> Gitea / Forgejo
      </button>
    </div>
  );
  return (
    <>
      <div className="page-head">
        <div>
          <h1>Git sources</h1>
          <div className="sub">Connect GitHub, GitLab or Gitea to pick repositories when creating apps, and deploy each push automatically.</div>
        </div>
        {buttons}
      </div>
      {sources.isPending ? (
        <Loading />
      ) : sources.error ? (
        <ErrorBox error={sources.error} />
      ) : sources.data.sources.length === 0 ? (
        <div className="card empty">
          <h3>No git sources yet</h3>
          <p>
            For GitHub, Basalt creates a GitHub App on your account or organization; for GitLab and Gitea (or Forgejo), you give
            it an access token. Either way it adds what each app needs to its repository: no keys or webhooks to copy.
          </p>
        </div>
      ) : (
        <div className="stack">
          {sources.data.sources.map((s) => (s.kind === "github" ? <SourceCard key={s.id} source={s} /> : <ForgeCard key={s.id} source={s} />))}
        </div>
      )}
      {connecting === "github" && <ConnectModal panelURL={sources.data?.panel_url ?? ""} onClose={() => setConnecting(null)} />}
      {(connecting === "gitlab" || connecting === "gitea") && (
        <ForgeModal kind={connecting} panelURL={sources.data?.panel_url ?? ""} onClose={() => setConnecting(null)} />
      )}
    </>
  );
}

function SourceCard({ source }: { source: GitSource }) {
  const qc = useQueryClient();
  const toast = useToast();
  const confirm = useConfirm();
  const detail = useQuery({ queryKey: ["git-source", source.id], queryFn: () => api.gitSource(source.id) });
  const remove = async () => {
    const ok = await confirm({
      title: `Remove ${source.name}?`,
      body: (
        <p>
          Apps built from its repositories keep their URL but can no longer fetch private ones, and pushes stop deploying them.
          Delete the GitHub App on GitHub too ({source.html_url}).
        </p>
      ),
      confirm: "Remove",
      danger: true,
    });
    if (!ok) return;
    try {
      await api.deleteGitSource(source.id);
      qc.invalidateQueries({ queryKey: ["git-sources"] });
    } catch (e) {
      toast(errorMessage(e), true);
    }
  };
  const insts = detail.data?.installations ?? [];
  return (
    <div className="card card-body stack">
      <div className="row wrap" style={{ justifyContent: "space-between" }}>
        <div>
          <div className="row">
            <h2>{source.name}</h2>
            <span className="badge plain">GitHub</span>
          </div>
          <div className="faint small">
            GitHub App of {source.owner} · connected {formatDate(source.created_at)} ·{" "}
            <a href={source.html_url} target="_blank" rel="noreferrer noopener">
              settings on GitHub
            </a>
          </div>
        </div>
        <div className="row">
          <a className="btn sm primary" href={source.install_url}>
            <Icon name="plus" size={14} /> Choose repositories
          </a>
          <button className="btn sm icon ghost" title="Remove" onClick={remove}>
            <Icon name="trash" size={14} />
          </button>
        </div>
      </div>
      {detail.isPending ? (
        <Loading />
      ) : detail.data?.error ? (
        <div className="alert err small">GitHub: {detail.data.error}</div>
      ) : insts.length === 0 ? (
        <div className="alert warn small">
          Not installed anywhere yet: choose the repositories it may read with “Choose repositories”.
        </div>
      ) : (
        <p className="muted small" style={{ margin: 0 }}>
          Installed on{" "}
          {insts.map((i, n) => (
            <span key={i.id}>
              {n > 0 && ", "}
              <a href={i.html_url} target="_blank" rel="noreferrer noopener">
                {i.account}
              </a>{" "}
              ({i.all ? "all repositories" : "chosen repositories"})
            </span>
          ))}
          . Create an app from one under <Link to="/apps">Apps</Link>.
        </p>
      )}
      {insts.length > 0 &&
        (insts.every((i) => i.statuses && i.pull_requests) ? (
          <p className="faint small" style={{ margin: 0 }}>
            Each deployment of a pushed commit shows on it in GitHub (“Basalt / app”: deploying, deployed or failed), linking to
            the app here; pull requests can get previews (an app's Overview).
          </p>
        ) : (
          <div className="alert small">
            This GitHub App was created before Basalt asked for{" "}
            {[
              !insts.every((i) => i.statuses) && "commit statuses (deployments shown on the commits)",
              !insts.every((i) => i.pull_requests) && "pull requests (previews)",
            ]
              .filter(Boolean)
              .join(" and ")}
            . On GitHub: Settings (of the account or organization that owns it) → Developer settings → GitHub Apps → {source.name}{" "}
            → Permissions & events: Repository permissions → Commit statuses: Read and write, Pull requests: Read-only; Subscribe to
            events → Pull request. Save, then accept the new permissions on each installation (GitHub asks there).
          </div>
        ))}
    </div>
  );
}

// ConnectModal creates a GitHub App: the manifest is posted to GitHub by
// the browser, GitHub sends the user back with a code.
function ConnectModal({ panelURL, onClose }: { panelURL: string; onClose: () => void }) {
  const host = panelURL ? new URL(panelURL).hostname : window.location.hostname;
  const [name, setName] = useState(`Basalt ${host}`.slice(0, 34));
  const [org, setOrg] = useState("");
  const form = useRef<HTMLFormElement>(null);
  const [go, setGo] = useState<{ action: string; manifest: string } | null>(null);
  const start = useMutation({
    mutationFn: () => api.githubManifest({ name: name.trim(), organization: org.trim() }),
    onSuccess: (r) => setGo(r),
  });
  useEffect(() => {
    if (go) form.current?.submit();
  }, [go]);
  return (
    <Modal
      title="Connect GitHub"
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" form="connect-github" disabled={start.isPending || !!go || !panelURL}>
            Continue on GitHub
          </button>
        </>
      }
    >
      <form
        id="connect-github"
        className="form"
        onSubmit={(e: FormEvent) => {
          e.preventDefault();
          start.mutate();
        }}
      >
        <ErrorBox error={start.error} />
        {!panelURL && (
          <div className="alert warn small">
            Set the panel's address first under <Link to="/notifications">Notifications</Link> (Links in messages): GitHub
            sends you back there and delivers pushes to it, so it must be reachable from the internet.
          </div>
        )}
        <Field label="Name of the GitHub App" help="Unique on GitHub; shown on your repositories' settings.">
          <input className="input" value={name} onChange={(e) => setName(e.target.value)} maxLength={34} required />
        </Field>
        <Field label="Organization" help="Empty: your own GitHub account. Otherwise you must be an owner of the organization.">
          <input className="input mono" value={org} onChange={(e) => setOrg(e.target.value)} placeholder="my-company" />
        </Field>
        <p className="faint small" style={{ margin: 0 }}>
          GitHub asks you to confirm the app. It may read code and metadata only, and sends push events to {panelURL || "the panel"}.
          Its keys stay in the panel, encrypted.
        </p>
      </form>
      {go && (
        <form ref={form} method="post" action={go.action} style={{ display: "none" }}>
          <input type="hidden" name="manifest" value={go.manifest} />
        </form>
      )}
    </Modal>
  );
}

// GitHubCallback is where GitHub sends the user back with the code of the
// new app.
export function GitHubCallback() {
  const [params] = useSearchParams();
  const navigate = useNavigate();
  const qc = useQueryClient();
  const done = useRef(false);
  const complete = useMutation({
    mutationFn: () => api.githubComplete({ code: params.get("code") ?? "", state: params.get("state") ?? "" }),
    onSuccess: (r) => {
      qc.invalidateQueries({ queryKey: ["git-sources"] });
      // On to choosing the repositories it may read.
      window.location.href = r.source.install_url;
    },
  });
  useEffect(() => {
    if (!done.current) {
      done.current = true;
      complete.mutate();
    }
  }, [complete]);
  return (
    <div className="card card-body stack" style={{ maxWidth: 560 }}>
      <h2>Connecting GitHub…</h2>
      {complete.error ? (
        <>
          <ErrorBox error={complete.error} />
          <div>
            <button className="btn" onClick={() => navigate("/git-sources")}>
              Back to Git sources
            </button>
          </div>
        </>
      ) : (
        <Loading />
      )}
    </div>
  );
}

const forgeName = { gitlab: "GitLab", gitea: "Gitea / Forgejo" } as const;

// ForgeCard shows a GitLab or Gitea source.
function ForgeCard({ source }: { source: GitSource }) {
  const qc = useQueryClient();
  const toast = useToast();
  const confirm = useConfirm();
  const detail = useQuery({ queryKey: ["git-source", source.id], queryFn: () => api.gitSource(source.id) });
  const kind = source.kind as "gitlab" | "gitea";
  const remove = async () => {
    const ok = await confirm({
      title: `Remove ${source.name}?`,
      body: (
        <p>
          Apps from its repositories keep building with their deploy keys and deploying on push; Basalt can no longer list
          repositories, show deployments on commits or remove the keys and webhooks it added. Revoke the token there too.
        </p>
      ),
      confirm: "Remove",
      danger: true,
    });
    if (!ok) return;
    try {
      await api.deleteGitSource(source.id);
      qc.invalidateQueries({ queryKey: ["git-sources"] });
    } catch (e) {
      toast(errorMessage(e), true);
    }
  };
  return (
    <div className="card card-body stack">
      <div className="row wrap" style={{ justifyContent: "space-between" }}>
        <div>
          <div className="row">
            <h2>{source.name}</h2>
            <span className="badge plain">{forgeName[kind]}</span>
          </div>
          <div className="faint small">
            <a href={source.base_url} target="_blank" rel="noreferrer noopener" className="mono">
              {source.base_url}
            </a>{" "}
            · token of {source.owner} · connected {formatDate(source.created_at)}
          </div>
        </div>
        <button className="btn sm icon ghost" title="Remove" onClick={remove}>
          <Icon name="trash" size={14} />
        </button>
      </div>
      {detail.data?.error ? (
        <div className="alert err small">The token no longer works: {detail.data.error}</div>
      ) : (
        <p className="muted small" style={{ margin: 0 }}>
          Create an app from one of its repositories under <Link to="/apps">Apps</Link>: Basalt adds the app's read-only deploy
          key and a webhook to the repository, and shows each deployment on its commit; {kind === "gitlab" ? "merge" : "pull"} requests
          can get previews (an app's Overview).
        </p>
      )}
    </div>
  );
}

// ForgeModal connects a GitLab or Gitea instance with an access token.
function ForgeModal({ kind, panelURL, onClose }: { kind: "gitlab" | "gitea"; panelURL: string; onClose: () => void }) {
  const qc = useQueryClient();
  const [name, setName] = useState(kind === "gitlab" ? "GitLab" : "Gitea");
  const [url, setURL] = useState(kind === "gitlab" ? "https://gitlab.com" : "https://");
  const [token, setToken] = useState("");
  const [allowPrivate, setAllowPrivate] = useState(false);
  const save = useMutation({
    mutationFn: () => api.createGitSource({ kind, name: name.trim(), url: url.trim(), token: token.trim(), allow_private: allowPrivate }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["git-sources"] });
      onClose();
    },
  });
  return (
    <Modal
      title={`Connect ${forgeName[kind]}`}
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" form="forge-form" disabled={save.isPending || !panelURL}>
            {save.isPending ? "Checking…" : "Connect"}
          </button>
        </>
      }
    >
      <form
        id="forge-form"
        className="form"
        onSubmit={(e: FormEvent) => {
          e.preventDefault();
          save.mutate();
        }}
      >
        <ErrorBox error={save.error} />
        {!panelURL && (
          <div className="alert warn small">
            Set the panel's address first under <Link to="/notifications">Notifications</Link> (Links in messages): the
            repositories' webhooks call it, so it must be reachable from {forgeName[kind]}.
          </div>
        )}
        <Field label="Name">
          <input className="input" value={name} onChange={(e) => setName(e.target.value)} maxLength={34} required />
        </Field>
        <Field label="Address" help={kind === "gitlab" ? "https://gitlab.com, or your own GitLab." : "Your Gitea or Forgejo, e.g. https://git.example.com (or https://codeberg.org)."}>
          <input className="input mono" value={url} onChange={(e) => setURL(e.target.value)} required />
        </Field>
        <Field
          label="Access token"
          help={
            kind === "gitlab"
              ? "A personal, group or project access token with the api scope, of someone who is a Maintainer of the repositories (User settings → Access tokens)."
              : "A token with the write:repository and read:user scopes, of an admin of the repositories (Settings → Applications → Generate token)."
          }
        >
          <input className="input mono" type="password" value={token} onChange={(e) => setToken(e.target.value)} required autoComplete="off" />
        </Field>
        <label className="row small">
          <input type="checkbox" checked={allowPrivate} onChange={(e) => setAllowPrivate(e.target.checked)} />
          It is on a private network (allows private addresses and plain http)
        </label>
        <p className="faint small" style={{ margin: 0 }}>
          The token stays in the panel, encrypted: servers clone with each app's own read-only deploy key. Basalt uses it to list
          repositories, add and remove apps' deploy keys and webhooks, and set the status of deployed commits.
        </p>
      </form>
    </Modal>
  );
}

import { useState, type FormEvent } from "react";
import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate, useParams } from "react-router";
import { api, can, roleRank, type Project, type ProjectMember, type ProjectRole, type User } from "../api";
import { ErrorBox, Field, Icon, Loading, Modal, errorMessage, formatDate, useConfirm, useToast } from "../components/ui";
import { useProjects } from "../components/projects";

const roleText: Record<ProjectRole, string> = {
  viewer: "Viewer — sees the project's sites and apps",
  member: "Member — also deploys, manages files and backups",
  admin: "Admin — also creates, configures and deletes them, and manages members",
};

export function ProjectsPage({ user }: { user: User }) {
  const projects = useProjects();
  const [creating, setCreating] = useState(false);
  return (
    <>
      <div className="page-head">
        <div>
          <h1>Projects</h1>
          <div className="sub">Sites, apps, databases, DNS zones and health checks are grouped in projects; members get a role in each.</div>
        </div>
        {can(user, "admin") && (
          <button className="btn primary" onClick={() => setCreating(true)}>
            <Icon name="plus" /> New project
          </button>
        )}
      </div>
      {projects.isPending ? (
        <Loading />
      ) : projects.error ? (
        <ErrorBox error={projects.error} />
      ) : projects.data.projects.length === 0 ? (
        <div className="card empty">
          <h3>No projects</h3>
          <p>You are not a member of any project yet: ask an administrator to add you.</p>
        </div>
      ) : (
        <div className="card table-wrap">
          <table className="table">
            <thead>
              <tr>
                <th>Project</th>
                <th className="num">Resources</th>
                <th className="num hide-sm">Members</th>
                <th>Your role</th>
              </tr>
            </thead>
            <tbody>
              {projects.data.projects.map((p) => (
                <tr key={p.id}>
                  <td>
                    <Link to={`/projects/${p.id}`}>
                      <strong>{p.name}</strong>
                    </Link>
                    {p.default && <span className="badge plain" style={{ marginLeft: 6 }}>default</span>}
                    {p.description && <div className="faint small">{p.description}</div>}
                  </td>
                  <td className="num">{p.resources}</td>
                  <td className="num hide-sm">{p.members}</td>
                  <td>
                    <span className="badge plain">{p.role}</span>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      <p className="faint small">
        A role on the panel (viewer, member, admin, owner) counts in every project. Users with the role “user” see only the
        projects they are members of. New resources go to the project chosen when they are created; an administrator of
        the panel can move them.
      </p>
      {creating && <ProjectModal project={null} onClose={() => setCreating(false)} />}
    </>
  );
}

function ProjectModal({ project, onClose }: { project: Project | null; onClose: () => void }) {
  const qc = useQueryClient();
  const navigate = useNavigate();
  const [name, setName] = useState(project?.name ?? "");
  const [description, setDescription] = useState(project?.description ?? "");
  const save = useMutation({
    mutationFn: () => (project ? api.updateProject(project.id, { name, description }) : api.createProject({ name, description })),
    onSuccess: (r) => {
      qc.invalidateQueries({ queryKey: ["projects"] });
      qc.invalidateQueries({ queryKey: ["project", r.project.id] });
      if (!project) navigate(`/projects/${r.project.id}`);
      onClose();
    },
  });
  return (
    <Modal
      title={project ? `Edit ${project.name}` : "New project"}
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" form="project-form" disabled={save.isPending}>
            Save
          </button>
        </>
      }
    >
      <form
        id="project-form"
        className="form"
        onSubmit={(e: FormEvent) => {
          e.preventDefault();
          save.mutate();
        }}
      >
        <ErrorBox error={save.error} />
        <Field label="Name">
          <input className="input" value={name} onChange={(e) => setName(e.target.value)} maxLength={64} required autoFocus />
        </Field>
        <Field label="Description" help="Optional.">
          <textarea className="input" rows={2} value={description} onChange={(e) => setDescription(e.target.value)} maxLength={500} />
        </Field>
      </form>
    </Modal>
  );
}

export function ProjectPage({ user }: { user: User }) {
  const id = Number(useParams().id);
  const qc = useQueryClient();
  const navigate = useNavigate();
  const toast = useToast();
  const confirm = useConfirm();
  const data = useQuery({ queryKey: ["project", id], queryFn: () => api.project(id) });
  const [editing, setEditing] = useState(false);
  if (data.isPending) return <Loading />;
  if (data.error) return <ErrorBox error={data.error} />;
  const { project: p, members } = data.data;
  const manage = roleRank[p.role] >= roleRank.admin;
  const remove = async () => {
    const ok = await confirm({
      title: `Delete project ${p.name}?`,
      body: <p>Only an empty project can go: move or delete its sites, apps, zones and checks first. Its memberships end.</p>,
      confirm: "Delete",
      danger: true,
    });
    if (!ok) return;
    try {
      await api.deleteProject(p.id);
      qc.invalidateQueries({ queryKey: ["projects"] });
      navigate("/projects");
    } catch (e) {
      toast(errorMessage(e), true);
    }
  };
  return (
    <>
      <div className="page-head">
        <div>
          <div className="row">
            <h1>{p.name}</h1>
            {p.default && <span className="badge plain">default</span>}
          </div>
          <div className="sub">
            <Link to="/projects">Projects</Link> · {p.resources} resources · your role: {p.role}
          </div>
        </div>
        {can(user, "admin") && (
          <div className="row">
            <button className="btn" onClick={() => setEditing(true)}>
              <Icon name="edit" /> Edit
            </button>
            {!p.default && (
              <button className="btn icon ghost" title="Delete project" onClick={remove}>
                <Icon name="trash" />
              </button>
            )}
          </div>
        )}
      </div>
      <div className="stack">
        {p.description && <p className="muted" style={{ margin: 0, whiteSpace: "pre-line" }}>{p.description}</p>}
        <div className="row wrap">
          {[
            ["/sites", "Sites"],
            ["/apps", "Apps"],
            ["/databases", "Databases"],
            ["/dns", "DNS zones"],
            ["/monitoring", "Health checks"],
          ].map(([to, label]) => (
            <Link key={to} className="btn sm" to={`${to}?project=${p.id}`}>
              {label}
            </Link>
          ))}
        </div>
        <MembersCard project={p} members={members} manage={manage} me={user} />
        {manage && <ActivityCard projectId={p.id} />}
      </div>
      {editing && <ProjectModal project={p} onClose={() => setEditing(false)} />}
    </>
  );
}

function MembersCard({ project, members, manage, me }: { project: Project; members: ProjectMember[]; manage: boolean; me: User }) {
  const qc = useQueryClient();
  const toast = useToast();
  const confirm = useConfirm();
  const [email, setEmail] = useState("");
  const [role, setRole] = useState<ProjectRole>("member");
  const refresh = () => {
    qc.invalidateQueries({ queryKey: ["project", project.id] });
    qc.invalidateQueries({ queryKey: ["projects"] });
  };
  const add = useMutation({
    mutationFn: () => api.addProjectMember(project.id, email.trim(), role),
    onSuccess: () => {
      setEmail("");
      refresh();
    },
  });
  const change = async (m: ProjectMember, r: ProjectRole) => {
    try {
      await api.setProjectMember(project.id, m.user_id, r);
      refresh();
    } catch (e) {
      toast(errorMessage(e), true);
    }
  };
  const remove = async (m: ProjectMember) => {
    const ok = await confirm({
      title: `Remove ${m.name} from ${project.name}?`,
      body: <p>{m.user_id === me.id ? "You lose access to the project, unless your role on the panel gives it." : "They lose their role in this project."}</p>,
      confirm: "Remove",
      danger: true,
    });
    if (!ok) return;
    try {
      await api.removeProjectMember(project.id, m.user_id);
      refresh();
    } catch (e) {
      toast(errorMessage(e), true);
    }
  };
  return (
    <div className="card">
      <div className="card-head">
        <h2>Members</h2>
      </div>
      {members.length === 0 ? (
        <div className="card-body muted">No members yet: only roles on the panel reach this project.</div>
      ) : (
        <div className="table-wrap">
          <table className="table">
            <tbody>
              {members.map((m) => (
                <tr key={m.user_id}>
                  <td>
                    <strong>{m.name}</strong>
                    <div className="faint small">{m.email}</div>
                  </td>
                  <td>
                    {manage ? (
                      <select className="input" style={{ width: "auto" }} value={m.role} onChange={(e) => change(m, e.target.value as ProjectRole)}>
                        {(Object.keys(roleText) as ProjectRole[]).map((r) => (
                          <option key={r} value={r}>
                            {r}
                          </option>
                        ))}
                      </select>
                    ) : (
                      <span className="badge plain">{m.role}</span>
                    )}
                  </td>
                  <td className="muted small hide-sm">since {formatDate(m.created_at)}</td>
                  <td className="num">
                    {manage && (
                      <button className="btn sm icon ghost" title="Remove" onClick={() => remove(m)}>
                        <Icon name="trash" size={14} />
                      </button>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {manage && (
        <form
          className="card-body form"
          onSubmit={(e: FormEvent) => {
            e.preventDefault();
            add.mutate();
          }}
        >
          <ErrorBox error={add.error} />
          <div className="row wrap" style={{ alignItems: "flex-end" }}>
            <Field label="Add a member by email">
              <input className="input" type="email" value={email} onChange={(e) => setEmail(e.target.value)} placeholder="name@example.com" required />
            </Field>
            <Field label="Role">
              <select className="input" value={role} onChange={(e) => setRole(e.target.value as ProjectRole)}>
                {(Object.keys(roleText) as ProjectRole[]).map((r) => (
                  <option key={r} value={r}>
                    {roleText[r]}
                  </option>
                ))}
              </select>
            </Field>
            <button className="btn primary" disabled={add.isPending}>
              Add
            </button>
          </div>
          <p className="faint small" style={{ margin: 0 }}>
            The user must have an account on the panel (an administrator creates it, with the role “user” for someone who
            works only in projects).
          </p>
        </form>
      )}
    </div>
  );
}

// detailText shows an entry's details as key: value pairs.
const detailText = (d: Record<string, unknown>) =>
  Object.entries(d)
    .filter(([k, v]) => v !== "" && v !== null && k !== "project_id")
    .map(([k, v]) => `${k}: ${typeof v === "object" ? JSON.stringify(v) : String(v)}`)
    .join(" · ");

// ActivityCard is the audit log of the project's resources (its admins).
function ActivityCard({ projectId }: { projectId: number }) {
  const log = useInfiniteQuery({
    queryKey: ["project-audit", projectId],
    queryFn: ({ pageParam }) => api.projectAudit(projectId, pageParam),
    initialPageParam: 0,
    getNextPageParam: (last) => (last.more && last.entries.length ? last.entries[last.entries.length - 1].id : undefined),
  });
  const entries = log.data?.pages.flatMap((p) => p.entries) ?? [];
  return (
    <div className="card">
      <div className="card-head">
        <h2>Activity</h2>
      </div>
      {log.isPending ? (
        <Loading />
      ) : log.error ? (
        <div className="card-body">
          <ErrorBox error={log.error} />
        </div>
      ) : entries.length === 0 ? (
        <div className="empty small">Nothing recorded yet.</div>
      ) : (
        <div className="table-wrap">
          <table className="table">
            <tbody>
              {entries.map((e) => (
                <tr key={e.id}>
                  <td className="muted small" style={{ whiteSpace: "nowrap" }}>
                    {formatDate(e.at)}
                  </td>
                  <td className="small">{e.actor ? e.actor.name || `user #${e.actor.id}` : <span className="faint">panel</span>}</td>
                  <td className="mono small" style={{ whiteSpace: "nowrap" }}>
                    {e.action}
                  </td>
                  <td className="small muted" style={{ overflowWrap: "anywhere" }}>
                    {detailText(e.details)}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {log.hasNextPage && (
        <div className="card-body" style={{ textAlign: "center" }}>
          <button className="btn sm" onClick={() => log.fetchNextPage()} disabled={log.isFetchingNextPage}>
            {log.isFetchingNextPage ? "Loading…" : "Load older"}
          </button>
        </div>
      )}
    </div>
  );
}

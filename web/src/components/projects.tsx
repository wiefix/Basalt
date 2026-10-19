import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useSearchParams } from "react-router";
import { api, can, roleRank, type Project, type ProjectResource, type Role, type User } from "../api";
import { ErrorBox, Field, useToast } from "./ui";

// Sites, apps, databases, DNS zones and health checks belong to projects; a
// user's role in a project applies to them (a role on the panel counts in
// every project). These helpers give pages the role that applies.

export function useProjects() {
  return useQuery({ queryKey: ["projects"], queryFn: api.projects, staleTime: 30_000 });
}

// roleIn is what the user may do with a resource of the project.
export function roleIn(user: User, projects: Project[] | undefined, projectId: number | undefined): Role {
  const p = projects?.find((x) => x.id === projectId);
  if (p) return p.role;
  return can(user, "viewer") ? user.role : "user";
}

// useAs returns the user with the role that applies in a project, for pages
// about one resource (their checks use can(user, …)).
export function useAs(user: User, projectId: number | undefined): User {
  const projects = useProjects();
  return { ...user, role: roleIn(user, projects.data?.projects, projectId) };
}

// adminProjects are the projects the user may create resources in.
export const adminProjects = (projects: Project[] | undefined) =>
  (projects ?? []).filter((p) => roleRank[p.role] >= roleRank.admin);

// useCanCreate reports whether the user may create resources somewhere.
export function useCanCreate(user: User) {
  const projects = useProjects();
  return can(user, "admin") || adminProjects(projects.data?.projects).length > 0;
}

// ProjectSelect picks the project of a new resource among those the user
// administers; with only one it shows nothing. value 0 means the first one.
export function ProjectSelect({ value, onChange }: { value: number; onChange: (id: number) => void }) {
  const projects = useProjects();
  const list = adminProjects(projects.data?.projects);
  if (list.length <= 1) return null;
  return (
    <Field label="Project">
      <select className="input" value={value || list[0].id} onChange={(e) => onChange(Number(e.target.value))}>
        {list.map((p) => (
          <option key={p.id} value={p.id}>
            {p.name}
          </option>
        ))}
      </select>
    </Field>
  );
}

// useNewProject is the state of a ProjectSelect: the chosen project, or the
// first one the user administers.
export function useNewProject(): [number, (id: number) => void] {
  const projects = useProjects();
  const [id, setId] = useState(0);
  return [id || adminProjects(projects.data?.projects)[0]?.id || 0, setId];
}

// ProjectBadge names a resource's project once there are several.
export function ProjectBadge({ id }: { id: number }) {
  const projects = useProjects();
  const list = projects.data?.projects ?? [];
  const p = list.find((x) => x.id === id);
  if (list.length <= 1 || !p) return null;
  return (
    <Link to={`/projects/${p.id}`} className="badge plain" style={{ marginLeft: 6 }} title="Project">
      {p.name}
    </Link>
  );
}

// ServerLink names a server, linking to its page for those who may see it.
export function ServerLink({ id, name, user, className }: { id: number; name: string; user: User; className?: string }) {
  if (!can(user, "viewer")) return <span className={className}>{name}</span>;
  return (
    <Link to={`/servers/${id}`} className={className}>
      {name}
    </Link>
  );
}

// MoveProjectCard moves a resource to another project (admins of the panel).
export function MoveProjectCard({ kind, id, current, what }: { kind: ProjectResource; id: number; current: number; what: string }) {
  const qc = useQueryClient();
  const toast = useToast();
  const projects = useProjects();
  const [target, setTarget] = useState(current);
  const move = useMutation({
    mutationFn: () => api.moveToProject(kind, id, target),
    onSuccess: () => {
      qc.invalidateQueries();
      toast(`Moved to ${projects.data?.projects.find((p) => p.id === target)?.name ?? "the project"}`);
    },
  });
  const list = projects.data?.projects ?? [];
  if (list.length <= 1) return null;
  return (
    <div className="card card-body form">
      <h2>Project</h2>
      <ErrorBox error={move.error} />
      <Field label={`The project ${what} belongs to`} help="Its members get their project role on it.">
        <div className="row">
          <select className="input" value={target} onChange={(e) => setTarget(Number(e.target.value))}>
            {list.map((p) => (
              <option key={p.id} value={p.id}>
                {p.name}
              </option>
            ))}
          </select>
          <button className="btn" disabled={move.isPending || target === current} onClick={() => move.mutate()}>
            Move
          </button>
        </div>
      </Field>
    </div>
  );
}

// useProjectFilter is the ?project= of a list page (0: all projects).
export function useProjectFilter(): [number, (id: number) => void] {
  const [params, setParams] = useSearchParams();
  const id = Number(params.get("project")) || 0;
  return [
    id,
    (v: number) => {
      const next = new URLSearchParams(params);
      if (v) next.set("project", String(v));
      else next.delete("project");
      setParams(next, { replace: true });
    },
  ];
}

// ProjectFilter narrows a list to one project once there are several.
export function ProjectFilter({ value, onChange }: { value: number; onChange: (id: number) => void }) {
  const projects = useProjects();
  const list = projects.data?.projects ?? [];
  if (list.length <= 1) return null;
  return (
    <select className="input" style={{ width: "auto", minWidth: 160 }} value={value} onChange={(e) => onChange(Number(e.target.value))}>
      <option value={0}>All projects</option>
      {list.map((p) => (
        <option key={p.id} value={p.id}>
          {p.name}
        </option>
      ))}
    </select>
  );
}

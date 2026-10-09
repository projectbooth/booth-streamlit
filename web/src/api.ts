// The module's own API (internal/api/apps.go). URLs are relative: this page is served under
// /iframe/streamlit/ and booth-core forwards the rest, attaching the viewer's identity, so there is
// no token to handle here. The backend decides every permission; the UI only hides what would be
// refused anyway.

export type Role = "owner" | "editor" | "viewer";

export interface Me {
  subject: string;
  workspace: string;
  role: Role;
  canAuthor: boolean;
  /** Whether this install gives apps data access at all (ADR 0104/0107). */
  dataAccess: boolean;
  /** Whether the editor can list catalog datasets to declare as sources (needs data access). */
  catalogSources?: boolean;
}

/** A catalog dataset an owner may declare as an app's source. */
export interface Dataset {
  id: string;
  name: string;
  description: string;
  format: string;
}

export interface App {
  id: string;
  workspace: string;
  name: string;
  description: string;
  source?: string;
  /** The app's requirements.txt, installed into its pod on every start. */
  requirements?: string;
  /** Catalog dataset ids the owner declared: lineage only, never a restriction. */
  sources?: string[];
  shared: boolean;
  desiredState: "running" | "stopped";
  suspended: boolean;
  /** Whose read access the app uses for data, capped at viewer (ADR 0104). */
  owner: string;
  /** Set while core refuses to mint for the owner: the app runs, its data doesn't. */
  dataPausedReason?: string;
  /** What the lifecycle observes (internal/lifecycle). */
  status: { state: "stopped" | "suspended" | "starting" | "installing" | "running" | "failed"; reason?: string };
  createdBy: string;
  createdAt: string;
  updatedBy: string;
  updatedAt: string;
}

export interface AppInput {
  name: string;
  description: string;
  source: string;
  requirements: string;
  sources: string[];
  shared: boolean;
}

export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
  ) {
    super(message);
  }
}

async function call<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(path, {
    method,
    headers: body === undefined ? undefined : { "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (res.status === 204) return undefined as T;
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new ApiError(res.status, (data as { error?: string }).error ?? `Request failed (${res.status})`);
  return data as T;
}

export const api = {
  me: () => call<Me>("GET", "api/me"),
  list: () => call<{ apps: App[] }>("GET", "api/apps").then((r) => r.apps),
  get: (id: string) => call<App>("GET", `api/apps/${encodeURIComponent(id)}`),
  create: (input: AppInput) => call<App>("POST", "api/apps", input),
  update: (id: string, input: AppInput) => call<App>("PUT", `api/apps/${encodeURIComponent(id)}`, input),
  remove: (id: string) => call<void>("DELETE", `api/apps/${encodeURIComponent(id)}`),
  start: (id: string) => call<App>("POST", `api/apps/${encodeURIComponent(id)}/start`),
  takeOwnership: (id: string) => call<App>("POST", `api/apps/${encodeURIComponent(id)}/take-ownership`),
  stop: (id: string) => call<App>("POST", `api/apps/${encodeURIComponent(id)}/stop`),
  /** Catalog datasets readable by the caller (an owner), for declaring sources. */
  datasets: () => call<{ datasets: Dataset[]; total: number }>("GET", "api/catalog/datasets"),
};

/** Where an app opens: relative, so it stays under the module's iframe path. */
export const appURL = (id: string) => `apps/${encodeURIComponent(id)}/`;

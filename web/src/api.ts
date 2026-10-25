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
  /** Whether this install turns on app writes through booth-api (ADR 0117). */
  writes?: boolean;
  /** Whether this install turns on app uploads to booth-storage (ADR 0117). */
  uploads?: boolean;
  /** Whether the release's app egress is open, so declaring a write or upload target closes this app's. */
  egressOpen?: boolean;
}

export type WriteOperation = "insert" | "update" | "delete";

/** One API an app may write to (ADR 0117): booth-api's slug and the operations. */
export interface WriteTarget {
  api: string;
  operations: WriteOperation[];
}

/** One Storage folder an app may add files to (ADR 0117): a backend id and a prefix ("" or ending in "/"). */
export interface UploadTarget {
  backend: string;
  prefix: string;
}

/** A declared target the viewer's own write or upload just used (GET api/apps/{id}/uses). */
export type Use = ({ kind: "write"; api: string } | { kind: "upload"; backend: string; prefix: string }) & { at: string };

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
  /** The owner's write declaration: disclosed to every viewer around the app (ADR 0117). */
  writeTargets?: WriteTarget[];
  /** The owner's upload declaration: shown with the write targets (ADR 0117). */
  uploadTargets?: UploadTarget[];
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
  /** Replace the app's write and upload declaration; adding a target, operation or folder needs confirmed. */
  setWriteTargets: (id: string, targets: WriteTarget[], uploads: UploadTarget[], confirmed: boolean) =>
    call<App>("PUT", `api/apps/${encodeURIComponent(id)}/write-targets`, { targets, uploads, confirmed }),
  /** The declared targets the caller's own writes and uploads used recently, and the server's time. */
  uses: (id: string) => call<{ now: string; uses: Use[] }>("GET", `api/apps/${encodeURIComponent(id)}/uses`),
  /** Catalog datasets readable by the caller (an owner), for declaring sources. */
  datasets: () => call<{ datasets: Dataset[]; total: number }>("GET", "api/catalog/datasets"),
};

/** Where an app opens: relative, so it stays under the module's iframe path. */
export const appURL = (id: string) => `apps/${encodeURIComponent(id)}/`;

/** The principal booth-api's grants name for an app: its token's literal subject (ADR 0117). */
export const principalOf = (workspace: string, id: string) => `streamlit:${workspace}:${id}`;

/**
 * booth-api's page in the shell, with the app's principal and the API filled in for the grant
 * (ADR 0117 item 1a). Absolute and opened in the top window: the shell, not this iframe, shows it.
 */
export const grantURL = (workspace: string, id: string, apiSlug: string) =>
  `/apis?${new URLSearchParams({ writeApi: apiSlug, writePrincipal: principalOf(workspace, id) }).toString()}`;

/**
 * booth-storage's Admin page in the shell, on its App uploads panel, with the app's principal, the
 * backend and the folder filled in (booth-storage docs/contracts/upload-grants.md, "URL prefill").
 * The path is booth-storage's adminNavPath. Opened in the top window, like grantURL.
 */
export const uploadGrantURL = (workspace: string, id: string, t: UploadTarget) =>
  `/storage/admin?${new URLSearchParams({ panel: "upload-grants", principal: principalOf(workspace, id), backendId: t.backend, prefix: t.prefix }).toString()}`;

/** A folder as the backend stores it (internal/apps CleanUploadPrefix): no leading "/", "" or ending in "/". */
export const cleanPrefix = (p: string) => {
  const s = p.trim().replace(/^\/+/, "").replace(/\/+$/, "");
  return s === "" ? "" : s + "/";
};

/** Whether an app declares anything it can change: it then opens in the module's frame page and has no internet. */
export const changesData = (a: App) => (a.writeTargets?.length ?? 0) + (a.uploadTargets?.length ?? 0) > 0;

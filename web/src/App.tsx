import { useEffect, useRef, useState } from "react";
import { api, appURL, changesData, type App as BoothApp, type Me, type Use } from "./api";
import { AppEditor } from "./AppEditor";
import { ErrorText, SHARED_DATA_WORDING, TrustNote, WriteAllowedPrompt, WritesDisclosure, targetKey, targetsWording } from "./components";
import { useAsync } from "./useAsync";

// One page, no client-side routes: every API URL is relative to this page's own path, so it must
// stay at the module root (/iframe/streamlit/). Opening an app navigates the frame to apps/<id>/,
// except an app that can write or upload (ADR 0117): that one opens at #app=<id>, this page with the
// app in a frame under a slim header whose Permissions control says what it can change.
type View = { kind: "list" } | { kind: "new" } | { kind: "edit"; id: string };

const frameHash = (id: string) => `#app=${encodeURIComponent(id)}`;

function framedApp(): string | null {
  const m = /^#app=([^&]+)$/.exec(window.location.hash);
  return m ? decodeURIComponent(m[1]) : null;
}

/** Where an app opens: in the frame page if it can write or upload, otherwise directly. */
export const openURL = (a: BoothApp) => (changesData(a) ? frameHash(a.id) : appURL(a.id));

export function App() {
  const [me] = useAsync(api.me, []);
  const [view, setView] = useState<View>({ kind: "list" });
  const [framed, setFramed] = useState(framedApp);
  useEffect(() => {
    const on = () => setFramed(framedApp());
    window.addEventListener("hashchange", on);
    return () => window.removeEventListener("hashchange", on);
  }, []);

  if (framed) return <AppFrame key={framed} id={framed} />;
  if (me.status === "loading") return <Shell><p className="text-sm">Loading…</p></Shell>;
  if (me.status === "error") return <Shell><ErrorText error={me.error} /></Shell>;

  return (
    <Shell>
      {view.kind === "list" && <AppList me={me.data} onNew={() => setView({ kind: "new" })} onEdit={(id) => setView({ kind: "edit", id })} />}
      {view.kind !== "list" && (
        <AppEditor id={view.kind === "edit" ? view.id : undefined} me={me.data} onDone={() => setView({ kind: "list" })} />
      )}
    </Shell>
  );
}

/**
 * An app that can write or upload, in a frame on this page (ADR 0117 item 2). The page, not the app,
 * discloses what it can change, in two places: the header's Permissions control (collapsed; any
 * viewer can open it on any visit) and the "Write allowed" prompt the first time in a browser
 * session that the viewer's own write or upload uses a target. The app is same-origin, so this is a
 * disclosure, not a barrier (ARCHITECTURE.md item 55; only workspace owners author apps).
 */
function AppFrame({ id }: { id: string }) {
  const [app] = useAsync(() => api.get(id), [id]);
  const [open, setOpen] = useState(false);
  const [used, dismiss] = useNewUses(app.status === "success" && changesData(app.data) ? app.data : null);
  if (app.status === "loading") return <Shell><p className="text-sm">Loading…</p></Shell>;
  if (app.status === "error") return <Shell><ErrorText error={app.error} /></Shell>;
  const targets = app.data.writeTargets ?? [];
  const uploads = app.data.uploadTargets ?? [];
  return (
    <div className="relative flex h-screen flex-col bg-white text-gray-900 dark:bg-gray-950 dark:text-gray-100">
      {changesData(app.data) && (
        <header className="flex items-center justify-between gap-4 border-b border-gray-200 px-4 py-1.5 text-sm dark:border-gray-800">
          <span className="truncate font-medium">{app.data.name}</span>
          <button type="button" aria-expanded={open} aria-controls="permissions" onClick={() => setOpen(!open)} className="rounded border border-gray-300 px-2 py-0.5 dark:border-gray-700">
            Permissions
          </button>
        </header>
      )}
      {open && (
        <div id="permissions" className="absolute right-4 top-10 z-20 w-96 max-w-[calc(100%-2rem)] rounded border border-gray-300 bg-white p-3 shadow-lg dark:border-gray-700 dark:bg-gray-900">
          <WritesDisclosure targets={targets} uploads={uploads} />
        </div>
      )}
      {used.length > 0 && !open && (
        <WriteAllowedPrompt used={used} targets={targets} onClose={dismiss} onShowAll={() => (dismiss(), setOpen(true))} />
      )}
      {/* The backend sends every other page load of this app here; it knows this frame by its
          referrer, so the policy is set rather than left to the defaults. */}
      <iframe title={app.data.name} src={appURL(id)} referrerPolicy="same-origin" className="w-full flex-1 border-0" />
    </div>
  );
}

/** How often the frame page asks which targets the viewer's writes used, while the tab is visible. */
export const USES_POLL_MS = 4000;

// Per app, in this browser session: when the session started (server time) and the targets already
// prompted. Uses from before the session aren't prompted; reloading the page doesn't prompt again.
type SessionUses = { since: string; seen: string[] };
const sessionKey = (id: string) => `booth.streamlit.uses.${id}`;

function readSession(id: string): SessionUses | null {
  try {
    const v = JSON.parse(sessionStorage.getItem(sessionKey(id)) ?? "null") as SessionUses | null;
    return v && typeof v.since === "string" && Array.isArray(v.seen) ? v : null;
  } catch {
    return null;
  }
}

function writeSession(id: string, v: SessionUses) {
  try {
    sessionStorage.setItem(sessionKey(id), JSON.stringify(v));
  } catch {
    // Storage blocked: the prompt may show again after a reload, which only repeats a disclosure.
  }
}

/**
 * The declared targets the viewer's own writes and uploads used for the first time this session,
 * and a function that marks them seen. Stops asking once every declared target has been prompted.
 */
function useNewUses(app: BoothApp | null): [Use[], () => void] {
  const id = app?.id;
  const [used, setUsed] = useState<Use[]>([]);
  const session = useRef<SessionUses | null>(null);
  const declared = app ? [...(app.writeTargets ?? []), ...(app.uploadTargets ?? [])].map(targetKey) : [];
  const allSeen = (s: SessionUses | null) => s !== null && declared.every((k) => s.seen.includes(k));
  const [done, setDone] = useState(false);
  useEffect(() => {
    if (!id || done) return;
    session.current ??= readSession(id);
    if (allSeen(session.current)) {
      setDone(true);
      return;
    }
    let live = true;
    const poll = () => {
      if (document.visibilityState === "hidden") return;
      api.uses(id).then(
        ({ now, uses }) => {
          if (!live) return;
          if (!session.current) {
            session.current = { since: now, seen: [] };
            writeSession(id, session.current);
          }
          const { since, seen } = session.current;
          const fresh = uses.filter((u) => Date.parse(u.at) > Date.parse(since) && declared.includes(targetKey(u)) && !seen.includes(targetKey(u)));
          if (fresh.length > 0) setUsed((prev) => [...prev, ...fresh.filter((u) => !prev.some((p) => targetKey(p) === targetKey(u)))]);
        },
        () => {
          // Asking again next time is enough: the Permissions control doesn't depend on this.
        },
      );
    };
    poll();
    const t = window.setInterval(poll, USES_POLL_MS);
    return () => {
      live = false;
      window.clearInterval(t);
    };
  }, [id, done]);
  const dismiss = () => {
    if (!id || !session.current) return;
    session.current = { ...session.current, seen: [...new Set([...session.current.seen, ...used.map(targetKey)])] };
    writeSession(id, session.current);
    setUsed([]);
    setDone(allSeen(session.current));
  };
  return [used, dismiss];
}

function Shell({ children }: { children: React.ReactNode }) {
  return <main className="min-h-screen bg-white p-6 text-gray-900 dark:bg-gray-950 dark:text-gray-100">{children}</main>;
}

function AppList({ me, onNew, onEdit }: { me: Me; onNew: () => void; onEdit: (id: string) => void }) {
  const [list, reload] = useAsync(api.list, []);
  const [actionError, setActionError] = useState<Error | null>(null);

  const act = (fn: () => Promise<unknown>) => {
    setActionError(null);
    fn().then(reload, setActionError);
  };

  return (
    <section className="space-y-4">
      <header className="flex items-center justify-between">
        <h1 className="text-xl font-semibold">Streamlit apps</h1>
        {me.canAuthor && (
          <button type="button" onClick={onNew} className="rounded bg-gray-900 px-3 py-1.5 text-sm text-white dark:bg-gray-100 dark:text-gray-900">
            New app
          </button>
        )}
      </header>
      <TrustNote />
      {!me.canAuthor && (
        <p className="text-sm text-gray-600 dark:text-gray-400">
          Only workspace owners can create or change apps. You can open apps shared with your workspace.
        </p>
      )}
      {actionError && <ErrorText error={actionError} />}
      {list.status === "loading" && <p className="text-sm">Loading…</p>}
      {list.status === "error" && <ErrorText error={list.error} />}
      {list.status === "success" && list.data.length === 0 && (
        <p className="text-sm text-gray-600 dark:text-gray-400">{me.canAuthor ? "No apps yet." : "No apps are shared with your workspace yet."}</p>
      )}
      {list.status === "success" && list.data.length > 0 && (
        <ul className="divide-y divide-gray-200 rounded border border-gray-200 dark:divide-gray-800 dark:border-gray-800">
          {list.data.map((a) => (
            <AppRow key={a.id} app={a} me={me} onEdit={() => onEdit(a.id)} act={act} />
          ))}
        </ul>
      )}
    </section>
  );
}

// "Sleeping" is idle shutdown: opening the app starts it again (internal/lifecycle).
const STATE_LABELS: Record<BoothApp["status"]["state"], string> = {
  stopped: "Stopped",
  suspended: "Sleeping",
  starting: "Starting",
  installing: "Installing packages",
  running: "Running",
  failed: "Failed",
};

function AppRow({ app, me, onEdit, act }: { app: BoothApp; me: Me; onEdit: () => void; act: (fn: () => Promise<unknown>) => void }) {
  const canAuthor = me.canAuthor;
  const running = app.desiredState === "running";
  const paused = Boolean(app.dataPausedReason);
  const label = STATE_LABELS[app.status?.state ?? (running ? "starting" : "stopped")];
  return (
    <li className="flex items-center justify-between gap-4 px-4 py-3">
      <div className="min-w-0">
        <div className="flex items-center gap-2">
          <span className="font-medium">{app.name}</span>
          <span className="rounded bg-gray-100 px-1.5 text-xs text-gray-700 dark:bg-gray-800 dark:text-gray-300">{label}</span>
          {canAuthor && (
            <span className="rounded bg-gray-100 px-1.5 text-xs text-gray-700 dark:bg-gray-800 dark:text-gray-300">
              {app.shared ? "Shared with workspace" : "Owners only"}
            </span>
          )}
          {me.dataAccess && paused && (
            <span className="rounded bg-amber-100 px-1.5 text-xs text-amber-900 dark:bg-amber-950 dark:text-amber-200">Data access paused</span>
          )}
        </div>
        {app.description && <p className="truncate text-sm text-gray-600 dark:text-gray-400">{app.description}</p>}
        {me.dataAccess && (
          <p className="text-xs text-gray-600 dark:text-gray-400">
            Reads data as {app.owner === me.subject ? "you" : app.owner}
            {app.shared && <> · {SHARED_DATA_WORDING}</>}
          </p>
        )}
        {changesData(app) && (
          <p className="text-xs text-sky-900 dark:text-sky-200">Can write to: {targetsWording(app.writeTargets ?? [], app.uploadTargets ?? [])}</p>
        )}
        {me.dataAccess && paused && (
          <p className="text-sm text-amber-900 dark:text-amber-200">
            Data access paused: {app.dataPausedReason}
            {canAuthor && " A workspace owner can take ownership to restore it."}
            {canAuthor && changesData(app) && " Taking ownership clears its write and upload targets and revokes its grants in the API and Storage modules."}
          </p>
        )}
        {canAuthor && app.status?.state === "failed" && app.status.reason && (
          // A pip failure is several lines of pip's output: keep its line breaks.
          <p className="whitespace-pre-wrap text-sm text-red-700 dark:text-red-400">Failed to start: {app.status.reason}</p>
        )}
      </div>
      <div className="flex shrink-0 gap-2 text-sm">
        <a href={openURL(app)} className="rounded border border-gray-300 px-2 py-1 dark:border-gray-700">Open</a>
        {canAuthor && (
          <>
            <button type="button" onClick={() => act(() => (running ? api.stop(app.id) : api.start(app.id)))} className="rounded border border-gray-300 px-2 py-1 dark:border-gray-700">
              {running ? "Stop" : "Start"}
            </button>
            <button type="button" onClick={onEdit} className="rounded border border-gray-300 px-2 py-1 dark:border-gray-700">Edit</button>
            {me.dataAccess && paused && (
              <button type="button" onClick={() => act(() => api.takeOwnership(app.id))} className="rounded border border-amber-400 px-2 py-1 text-amber-900 dark:border-amber-700 dark:text-amber-200">
                Take ownership
              </button>
            )}
          </>
        )}
      </div>
    </li>
  );
}

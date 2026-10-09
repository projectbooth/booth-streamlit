import { useState } from "react";
import { api, appURL, type App as BoothApp, type Me } from "./api";
import { AppEditor } from "./AppEditor";
import { ErrorText, SHARED_DATA_WORDING, TrustNote } from "./components";
import { useAsync } from "./useAsync";

// One page, no client-side routes: every API URL is relative to this page's own path, so it must
// stay at the module root (/iframe/streamlit/). Opening an app navigates the frame to apps/<id>/.
type View = { kind: "list" } | { kind: "new" } | { kind: "edit"; id: string };

export function App() {
  const [me] = useAsync(api.me, []);
  const [view, setView] = useState<View>({ kind: "list" });

  if (me.status === "loading") return <Shell><p className="text-sm">Loading…</p></Shell>;
  if (me.status === "error") return <Shell><ErrorText error={me.error} /></Shell>;

  return (
    <Shell>
      {view.kind === "list" && <AppList me={me.data} onNew={() => setView({ kind: "new" })} onEdit={(id) => setView({ kind: "edit", id })} />}
      {view.kind !== "list" && (
        <AppEditor id={view.kind === "edit" ? view.id : undefined} onDone={() => setView({ kind: "list" })} />
      )}
    </Shell>
  );
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
        {me.dataAccess && paused && (
          <p className="text-sm text-amber-900 dark:text-amber-200">
            Data access paused: {app.dataPausedReason}
            {canAuthor && " A workspace owner can take ownership to restore it."}
          </p>
        )}
        {canAuthor && app.status?.state === "failed" && app.status.reason && (
          // A pip failure is several lines of pip's output: keep its line breaks.
          <p className="whitespace-pre-wrap text-sm text-red-700 dark:text-red-400">Failed to start: {app.status.reason}</p>
        )}
      </div>
      <div className="flex shrink-0 gap-2 text-sm">
        <a href={appURL(app.id)} className="rounded border border-gray-300 px-2 py-1 dark:border-gray-700">Open</a>
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

import { useState } from "react";
import {
  api,
  cleanPrefix,
  grantURL,
  principalOf,
  uploadGrantURL,
  type App,
  type AppInput,
  type Dataset,
  type Me,
  type UploadTarget,
  type WriteOperation,
  type WriteTarget,
} from "./api";
import { ErrorText, SHARED_DATA_WORDING, SOURCES_WORDING, TrustNote, folderWording, opWords } from "./components";
import { useAsync } from "./useAsync";

const STARTER = `import streamlit as st
import booth_streamlit

viewer = booth_streamlit.user()
st.title("Hello")
st.write(f"Viewing as {viewer.subject if viewer else 'unknown'}")
`;

const empty: AppInput = { name: "", description: "", source: STARTER, requirements: "", sources: [], shared: false };

/** Create (no id) or edit an app. Owners only; the backend refuses anyone else. */
export function AppEditor({ id, me, onDone }: { id?: string; me?: Me; onDone: () => void }) {
  const [loaded] = useAsync(() => (id ? api.get(id) : Promise.resolve(null)), [id]);
  if (loaded.status === "loading") return <p className="text-sm">Loading…</p>;
  if (loaded.status === "error") return <ErrorText error={loaded.error} />;
  const a = loaded.data;
  const initial = a
    ? { name: a.name, description: a.description, source: a.source ?? "", requirements: a.requirements ?? "", sources: a.sources ?? [], shared: a.shared }
    : empty;
  return (
    <div className="space-y-8">
      <Form id={id} initial={initial} onDone={onDone} />
      {a && (me?.writes || me?.uploads) && <WriteTargets app={a} me={me} />}
    </div>
  );
}

function Form({ id, initial, onDone }: { id?: string; initial: AppInput; onDone: () => void }) {
  const [form, setForm] = useState(initial);
  const [error, setError] = useState<Error | null>(null);
  const [busy, setBusy] = useState(false);
  const [confirmDelete, setConfirmDelete] = useState(false);
  const set = <K extends keyof AppInput>(k: K, v: AppInput[K]) => setForm((f) => ({ ...f, [k]: v }));

  const run = (fn: () => Promise<unknown>) => {
    setBusy(true);
    setError(null);
    fn().then(onDone, (e: Error) => {
      setError(e);
      setBusy(false);
    });
  };

  const field = "w-full rounded border border-gray-300 bg-white px-2 py-1.5 text-sm dark:border-gray-700 dark:bg-gray-900";
  return (
    <form
      className="space-y-4"
      onSubmit={(e) => {
        e.preventDefault();
        run(() => (id ? api.update(id, form) : api.create(form)));
      }}
    >
      <h1 className="text-xl font-semibold">{id ? "Edit app" : "New app"}</h1>
      <TrustNote />
      <label className="block space-y-1 text-sm">
        <span>Name</span>
        <input className={field} value={form.name} maxLength={100} required onChange={(e) => set("name", e.target.value)} />
      </label>
      <label className="block space-y-1 text-sm">
        <span>Description</span>
        <input className={field} value={form.description} maxLength={2000} onChange={(e) => set("description", e.target.value)} />
      </label>
      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" checked={form.shared} onChange={(e) => set("shared", e.target.checked)} />
        <span>Shared with workspace members (otherwise only workspace owners can open it)</span>
      </label>
      {form.shared && <p className="text-sm text-gray-600 dark:text-gray-400">{SHARED_DATA_WORDING}</p>}
      <label className="block space-y-1 text-sm">
        <span>Python source</span>
        <textarea className={`${field} h-80 font-mono`} spellCheck={false} value={form.source} onChange={(e) => set("source", e.target.value)} />
      </label>
      <label className="block space-y-1 text-sm">
        <span>requirements.txt (optional)</span>
        <textarea
          className={`${field} h-24 font-mono`}
          spellCheck={false}
          maxLength={16384}
          placeholder="humanize==4.12.0"
          value={form.requirements}
          onChange={(e) => set("requirements", e.target.value)}
        />
        <span className="block text-xs text-gray-600 dark:text-gray-400">
          One package per line, no pip options. Installed every time the app starts, which makes starting slower.
        </span>
      </label>
      <Sources selected={form.sources} onChange={(v) => set("sources", v)} />
      {error && <ErrorText error={error} />}
      <div className="flex items-center gap-2 text-sm">
        <button type="submit" disabled={busy} className="rounded bg-gray-900 px-3 py-1.5 text-white disabled:opacity-50 dark:bg-gray-100 dark:text-gray-900">
          {id ? "Save" : "Create"}
        </button>
        <button type="button" onClick={onDone} className="rounded border border-gray-300 px-3 py-1.5 dark:border-gray-700">Cancel</button>
        {id && (
          <button
            type="button"
            disabled={busy}
            // Two clicks instead of a browser confirm() dialog.
            onClick={() => (confirmDelete ? run(() => api.remove(id)) : setConfirmDelete(true))}
            className="ml-auto rounded border border-red-300 px-3 py-1.5 text-red-700 dark:border-red-800 dark:text-red-400"
          >
            {confirmDelete ? "Click again to delete" : "Delete"}
          </button>
        )}
      </div>
    </form>
  );
}

/** Declared sources: catalog datasets, listed as the signed-in owner can read them. */
function Sources({ selected, onChange }: { selected: string[]; onChange: (v: string[]) => void }) {
  const [list] = useAsync(() => api.datasets(), []);
  const [filter, setFilter] = useState("");
  if (list.status === "loading") return <p className="text-sm">Loading catalog datasets…</p>;
  if (list.status === "error") {
    // 404: this install has no data access, so the list can't be read as you; nothing to declare.
    if ((list.error as { status?: number }).status === 404) return null;
    return <ErrorText error={list.error} />;
  }
  const known = new Map(list.data.datasets.map((d: Dataset) => [d.id, d]));
  const toggle = (id: string) => onChange(selected.includes(id) ? selected.filter((s) => s !== id) : [...selected, id]);
  const shown = list.data.datasets.filter((d) => d.name.toLowerCase().includes(filter.toLowerCase()));
  return (
    <fieldset className="space-y-2 text-sm">
      <legend>Sources (optional)</legend>
      <p className="text-xs text-gray-600 dark:text-gray-400">{SOURCES_WORDING}</p>
      {list.data.datasets.length > 10 && (
        <input className="w-full rounded border border-gray-300 bg-white px-2 py-1 dark:border-gray-700 dark:bg-gray-900" placeholder="Filter datasets" value={filter} onChange={(e) => setFilter(e.target.value)} />
      )}
      {list.data.datasets.length === 0 && <p className="text-xs">No catalog datasets you can read in this workspace.</p>}
      <ul className="max-h-48 space-y-1 overflow-auto">
        {selected
          .filter((id) => !known.has(id))
          .map((id) => (
            <li key={id}>
              <label className="flex items-center gap-2">
                <input type="checkbox" checked onChange={() => toggle(id)} />
                <span>{id} (not in the catalog, or not readable by you)</span>
              </label>
            </li>
          ))}
        {shown.map((d) => (
          <li key={d.id}>
            <label className="flex items-center gap-2">
              <input type="checkbox" checked={selected.includes(d.id)} onChange={() => toggle(d.id)} />
              <span>{d.name}</span>
              <span className="text-xs text-gray-500">{d.format}</span>
            </label>
          </li>
        ))}
      </ul>
      {list.data.total > list.data.datasets.length && (
        <p className="text-xs">Showing {list.data.datasets.length} of {list.data.total} datasets.</p>
      )}
    </fieldset>
  );
}

const OPS: WriteOperation[] = ["insert", "update", "delete"];

/** Whether next adds a target, an operation or a folder that saved doesn't have (the backend's rule too). */
function widens(saved: WriteTarget[], next: WriteTarget[], savedUploads: UploadTarget[], nextUploads: UploadTarget[]) {
  return (
    next.some((t) => {
      const cur = saved.find((s) => s.api === t.api);
      return !cur || t.operations.some((o) => !cur.operations.includes(o));
    }) || nextUploads.some((u) => !savedUploads.some((s) => s.backend === u.backend && s.prefix === u.prefix))
  );
}

/**
 * ADR 0117 item 1a: the app's side of a write and upload declaration. The owner also grants the app
 * on the API module's page (writes) and Storage's Admin page (uploads), with their own sign-in; this
 * module can't make those grants (it never holds a person's token). Each needs both. Saved on its
 * own, with the plain-words confirmation when it adds anything.
 */
function WriteTargets({ app, me }: { app: App; me: Me }) {
  const [saved, setSaved] = useState<{ targets: WriteTarget[]; uploads: UploadTarget[] }>({
    targets: app.writeTargets ?? [],
    uploads: app.uploadTargets ?? [],
  });
  const [targets, setTargets] = useState<WriteTarget[]>(saved.targets);
  const [uploads, setUploads] = useState<UploadTarget[]>(saved.uploads);
  const [slug, setSlug] = useState("");
  const [backend, setBackend] = useState("");
  const [folder, setFolder] = useState("");
  const [confirmed, setConfirmed] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<Error | null>(null);
  const [done, setDone] = useState(false);
  const principal = principalOf(app.workspace, app.id);
  const needsConfirm = widens(saved.targets, targets, saved.uploads, uploads);
  const dirty = JSON.stringify(saved) !== JSON.stringify({ targets, uploads });
  const empty = targets.some((t) => t.operations.length === 0);

  const changed = () => {
    setConfirmed(false);
    setDone(false);
  };
  const edit = (next: WriteTarget[]) => {
    setTargets(next);
    changed();
  };
  const editUploads = (next: UploadTarget[]) => {
    setUploads(next);
    changed();
  };
  const toggle = (apiSlug: string, op: WriteOperation) =>
    edit(
      targets.map((t) =>
        t.api !== apiSlug ? t : { ...t, operations: OPS.filter((o) => (o === op ? !t.operations.includes(o) : t.operations.includes(o))) },
      ),
    );
  const add = () => {
    const s = slug.trim();
    if (!s || targets.some((t) => t.api === s)) return;
    // Insert only by default; update and delete are opt-in (ADR 0117 item 5).
    edit([...targets, { api: s, operations: ["insert"] }]);
    setSlug("");
  };
  const addUpload = () => {
    const u = { backend: backend.trim(), prefix: cleanPrefix(folder) };
    if (!u.backend || uploads.some((x) => x.backend === u.backend && x.prefix === u.prefix)) return;
    editUploads([...uploads, u]);
    setBackend("");
    setFolder("");
  };
  const save = () => {
    setBusy(true);
    setError(null);
    // A kind this install turns off is never sent: the backend refuses it.
    api.setWriteTargets(app.id, me.writes ? targets : [], me.uploads ? uploads : [], confirmed).then(
      (a) => {
        const next = { targets: a.writeTargets ?? [], uploads: a.uploadTargets ?? [] };
        setSaved(next);
        setTargets(next.targets);
        setUploads(next.uploads);
        setConfirmed(false);
        setDone(true);
        setBusy(false);
      },
      (e: Error) => {
        setError(e);
        setBusy(false);
      },
    );
  };
  const enter = (fn: () => void) => (e: React.KeyboardEvent) => {
    if (e.key === "Enter") {
      e.preventDefault();
      fn();
    }
  };

  const field = "rounded border border-gray-300 bg-white px-2 py-1 text-sm dark:border-gray-700 dark:bg-gray-900";
  const row = "flex flex-wrap items-center gap-3 rounded border border-gray-200 px-3 py-2 dark:border-gray-800";
  const removeButton = "ml-auto rounded border border-gray-300 px-2 py-0.5 dark:border-gray-700";
  const plainChanges = [
    ...targets.map((t) => opWords(t.operations) + " rows through " + t.api),
    ...uploads.map((u) => "add files to " + folderWording(u)),
  ].join("; ");
  return (
    <section aria-labelledby="write-targets" className="space-y-3 text-sm">
      <h2 id="write-targets" className="text-base font-semibold">Write targets</h2>
      {me.egressOpen && (
        <p className="rounded border border-amber-300 bg-amber-50 px-3 py-2 text-amber-900 dark:border-amber-700 dark:bg-amber-950 dark:text-amber-200">
          An app with any write or upload target has no internet access: it can&apos;t call outside services, and its requirements.txt can install
          packages only from a package index inside the cluster, if this install has one. Saving the first target or removing the last restarts the
          app.
        </p>
      )}
      {targets.length === 0 && uploads.length === 0 && <p className="text-gray-600 dark:text-gray-400">None. This app can only read.</p>}

      {me.writes && (
        <div className="space-y-2">
          <h3 className="font-medium">Tables, through the API module</h3>
          <p className="text-gray-700 dark:text-gray-300">
            Let this app add, change or delete rows in tables served by the API module. A target here is only half of it: an owner also has to grant
            this app on the API&apos;s page (API, then the API, then Writes), as <span className="font-mono text-xs">{principal}</span>. A write needs
            both.
          </p>
          <ul className="space-y-2">
            {targets.map((t) => (
              <li key={t.api} className={row}>
                <span className="font-mono">{t.api}</span>
                {OPS.map((op) => (
                  <label key={op} className="flex items-center gap-1">
                    <input type="checkbox" checked={t.operations.includes(op)} onChange={() => toggle(t.api, op)} />
                    <span>{opWords([op])}</span>
                  </label>
                ))}
                <a href={grantURL(app.workspace, app.id, t.api)} target="_top" className="underline">
                  Grant on the API page
                </a>
                <button type="button" onClick={() => edit(targets.filter((x) => x.api !== t.api))} className={removeButton}>
                  Remove
                </button>
              </li>
            ))}
          </ul>
          <div className="flex gap-2">
            <input
              className={field}
              placeholder="API slug, as shown on the API page"
              aria-label="API slug"
              value={slug}
              onChange={(e) => setSlug(e.target.value)}
              onKeyDown={enter(add)}
            />
            <button type="button" onClick={add} className="rounded border border-gray-300 px-2 py-1 dark:border-gray-700">
              Add
            </button>
          </div>
        </div>
      )}

      {me.uploads && (
        <div className="space-y-2">
          <h3 className="font-medium">Files, into Storage folders</h3>
          <p className="text-gray-700 dark:text-gray-300">
            Let this app add files to a Storage folder. It can only add new files: it can&apos;t replace, delete or list them, and each file&apos;s name
            gets the upload time added. An owner also has to grant this app on Storage&apos;s Admin page (App uploads), as{" "}
            <span className="font-mono text-xs">{principal}</span>; that grant sets which file types and sizes are allowed. An upload needs both.
          </p>
          <ul className="space-y-2">
            {uploads.map((u) => (
              <li key={u.backend + ":" + u.prefix} className={row}>
                <span className="font-mono">{folderWording(u)}</span>
                <a href={uploadGrantURL(app.workspace, app.id, u)} target="_top" className="underline">
                  Grant on the Storage page
                </a>
                <button
                  type="button"
                  onClick={() => editUploads(uploads.filter((x) => !(x.backend === u.backend && x.prefix === u.prefix)))}
                  className={removeButton}
                >
                  Remove
                </button>
              </li>
            ))}
          </ul>
          <div className="flex flex-wrap gap-2">
            <input
              className={field}
              placeholder="Backend id, as on the Storage page"
              aria-label="Storage backend"
              value={backend}
              onChange={(e) => setBackend(e.target.value)}
              onKeyDown={enter(addUpload)}
            />
            <input
              className={field}
              placeholder="Folder, such as finance/inbox/"
              aria-label="Folder"
              value={folder}
              onChange={(e) => setFolder(e.target.value)}
              onKeyDown={enter(addUpload)}
            />
            <button type="button" onClick={addUpload} className="rounded border border-gray-300 px-2 py-1 dark:border-gray-700">
              Add folder
            </button>
          </div>
        </div>
      )}

      {empty && <p role="alert" className="text-red-700 dark:text-red-400">Every target needs at least one operation, or remove it.</p>}
      {needsConfirm && !empty && (
        <label className="flex items-start gap-2 rounded border border-amber-300 bg-amber-50 px-3 py-2 text-amber-900 dark:border-amber-700 dark:bg-amber-950 dark:text-amber-200">
          <input type="checkbox" checked={confirmed} onChange={(e) => setConfirmed(e.target.checked)} className="mt-1" />
          <span>
            Anyone who can open this app, including viewers, will be able to {plainChanges}. The change is made as the app, not as them. Who made it
            is recorded only if the app&apos;s code records it.
          </span>
        </label>
      )}
      {error && <ErrorText error={error} />}
      {done && !dirty && (
        <p role="status" className="text-gray-700 dark:text-gray-300">
          Saved. Without an owner&apos;s grant on the API page (writes) or the Storage page (uploads), this app&apos;s changes are still refused.
        </p>
      )}
      <button
        type="button"
        disabled={busy || !dirty || empty || (needsConfirm && !confirmed)}
        onClick={save}
        className="rounded bg-gray-900 px-3 py-1.5 text-white disabled:opacity-50 dark:bg-gray-100 dark:text-gray-900"
      >
        Save write targets
      </button>
    </section>
  );
}

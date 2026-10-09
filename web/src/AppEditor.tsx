import { useState } from "react";
import { api, type AppInput, type Dataset } from "./api";
import { ErrorText, SHARED_DATA_WORDING, SOURCES_WORDING, TrustNote } from "./components";
import { useAsync } from "./useAsync";

const STARTER = `import streamlit as st
import booth_streamlit

viewer = booth_streamlit.user()
st.title("Hello")
st.write(f"Viewing as {viewer.subject if viewer else 'unknown'}")
`;

const empty: AppInput = { name: "", description: "", source: STARTER, requirements: "", sources: [], shared: false };

/** Create (no id) or edit an app. Owners only; the backend refuses anyone else. */
export function AppEditor({ id, onDone }: { id?: string; onDone: () => void }) {
  const [loaded] = useAsync(() => (id ? api.get(id) : Promise.resolve(null)), [id]);
  if (loaded.status === "loading") return <p className="text-sm">Loading…</p>;
  if (loaded.status === "error") return <ErrorText error={loaded.error} />;
  const a = loaded.data;
  const initial = a
    ? { name: a.name, description: a.description, source: a.source ?? "", requirements: a.requirements ?? "", sources: a.sources ?? [], shared: a.shared }
    : empty;
  return <Form id={id} initial={initial} onDone={onDone} />;
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

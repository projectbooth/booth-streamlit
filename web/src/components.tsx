import { ApiError, type UploadTarget, type Use, type WriteOperation, type WriteTarget } from "./api";

/**
 * ADR 0105: an app's code runs in the viewer's browser with the same access as the shell page
 * (ARCHITECTURE.md item 55), so the viewer has to trust whoever wrote it.
 */
export function TrustNote() {
  return (
    <p role="note" className="rounded border border-amber-300 bg-amber-50 px-3 py-2 text-sm text-amber-900 dark:border-amber-700 dark:bg-amber-950 dark:text-amber-200">
      App code runs in your browser with access to your Booth session. Only open apps from workspace owners you trust.
    </p>
  );
}

/** ADR 0107 item 4: what a shared app can read, in plain words. */
export const SHARED_DATA_WORDING = "This app can read any data you can read in this workspace.";

/** ADR 0107 item 4: declared sources are lineage, not a limit. */
export const SOURCES_WORDING =
  "Sources tell the catalog what this app reads, so it can show where the app's data comes from. They don't limit what the app can read: it can still read any data its owner can read in this workspace.";

export function ErrorText({ error }: { error: Error }) {
  const msg = error instanceof ApiError && error.status === 403 ? "Only workspace owners can do that." : error.message;
  return <p role="alert" className="text-sm text-red-700 dark:text-red-400">{msg}</p>;
}

const OP_WORDS: Record<WriteOperation, string> = { insert: "add", update: "change", delete: "delete" };

/** "add, change" for a target's operations, in plain words. */
export const opWords = (ops: WriteOperation[]) => ops.map((o) => OP_WORDS[o]).join(", ");

/** "local:finance/inbox/", or "local (top folder)" for a backend's root. */
export const folderWording = (t: UploadTarget) => (t.prefix ? `${t.backend}:${t.prefix}` : `${t.backend} (top folder)`);

/** "household (add, change) · notes (add) · upload to local:finance/inbox/". */
export const targetsWording = (targets: WriteTarget[], uploads: UploadTarget[] = []) =>
  [...targets.map((t) => `${t.api} (${opWords(t.operations)})`), ...uploads.map((u) => `upload to ${folderWording(u)}`)].join(" · ");

/**
 * ADR 0117 item 2: what an app can write and upload, in plain words. Rendered by this module's page,
 * never by the app: in the header's Permissions control, which every viewer can open on every visit.
 */
export function WritesDisclosure({ targets, uploads = [] }: { targets: WriteTarget[]; uploads?: UploadTarget[] }) {
  return (
    <p role="note" aria-label="What this app can change" className="text-sm">
      This app can write to: {targetsWording(targets, uploads)}. Anyone who opens it can make these changes, as the app.
    </p>
  );
}

/** What a target the viewer just used allows, in plain words, for the "Write allowed" prompt. */
export const usedWording = (u: Use, targets: WriteTarget[]) => {
  if (u.kind === "upload") return `This app just added a file to ${folderWording(u)}.`;
  const ops = targets.find((t) => t.api === u.api)?.operations ?? [];
  return `This app just saved a change to ${u.api}. It can ${opWords(ops)} rows there.`;
};

/**
 * The "Write allowed" prompt: shown by this module's page, outside the app's frame, the first time
 * in a browser session that the viewer's own write or upload uses a declared target. It stays until
 * dismissed.
 */
export function WriteAllowedPrompt({ used, targets, onShowAll, onClose }: { used: Use[]; targets: WriteTarget[]; onShowAll: () => void; onClose: () => void }) {
  return (
    <div role="dialog" aria-label="Write allowed" className="absolute right-4 top-12 z-10 w-96 max-w-[calc(100%-2rem)] space-y-2 rounded border border-sky-300 bg-sky-50 p-3 text-sm text-sky-950 shadow-lg dark:border-sky-800 dark:bg-sky-950 dark:text-sky-100">
      <p className="font-semibold">Write allowed</p>
      {used.map((u) => (
        <p key={targetKey(u)}>{usedWording(u, targets)}</p>
      ))}
      <p>The change is made as the app, not as you. Anyone who opens this app can make these changes.</p>
      <div className="flex justify-end gap-2">
        <button type="button" onClick={onShowAll} className="rounded border border-sky-400 px-2 py-1 dark:border-sky-700">See all permissions</button>
        <button type="button" onClick={onClose} className="rounded bg-sky-900 px-2 py-1 text-white dark:bg-sky-100 dark:text-sky-950">OK</button>
      </div>
    </div>
  );
}

/** One key per declared target: "write:household", "upload:local:finance/inbox/". */
export const targetKey = (u: Use | WriteTarget | UploadTarget) =>
  "api" in u ? `write:${u.api}` : `upload:${u.backend}:${u.prefix}`;

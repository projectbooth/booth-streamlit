import { ApiError, type UploadTarget, type WriteOperation, type WriteTarget } from "./api";

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
 * ADR 0117 item 2: every viewer sees what an app can write and upload, above the app, on every visit.
 * Rendered by this module's page, never by the app, and it can't be dismissed.
 */
export function WritesHeader({ targets, uploads = [] }: { targets: WriteTarget[]; uploads?: UploadTarget[] }) {
  return (
    <p role="note" aria-label="What this app can change" className="border-b border-sky-300 bg-sky-50 px-4 py-2 text-sm text-sky-950 dark:border-sky-800 dark:bg-sky-950 dark:text-sky-100">
      This app can write to: {targetsWording(targets, uploads)}. Anyone who opens it can make these changes, as the app.
    </p>
  );
}

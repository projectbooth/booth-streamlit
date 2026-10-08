import { ApiError } from "./api";

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

export function ErrorText({ error }: { error: Error }) {
  const msg = error instanceof ApiError && error.status === 403 ? "Only workspace owners can do that." : error.message;
  return <p role="alert" className="text-sm text-red-700 dark:text-red-400">{msg}</p>;
}

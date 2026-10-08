import { useCallback, useEffect, useState } from "react";

// A small idle/loading/success/error state machine for one request, the pattern the platform's
// modules use instead of a data-fetching library (ARCHITECTURE.md §6).
export type AsyncState<T> =
  | { status: "loading" }
  | { status: "success"; data: T }
  | { status: "error"; error: Error };

export function useAsync<T>(fn: () => Promise<T>, deps: unknown[]): [AsyncState<T>, () => void] {
  const [state, setState] = useState<AsyncState<T>>({ status: "loading" });
  const [tick, setTick] = useState(0);
  const reload = useCallback(() => setTick((t) => t + 1), []);
  useEffect(() => {
    let live = true;
    setState({ status: "loading" });
    fn().then(
      (data) => live && setState({ status: "success", data }),
      (error: Error) => live && setState({ status: "error", error }),
    );
    return () => {
      live = false;
    };
  }, [...deps, tick]);
  return [state, reload];
}

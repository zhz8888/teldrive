import type { QueryClient } from "@tanstack/react-query";
import { $api, fetchClient } from "./client";

/**
 * The shared `$api` client under a task-oriented name, so a job screen can bind
 * hooks without reaching for the generic client. It has no importer today: the
 * job routes use `fetchClient` for calls and `invalidateTaskQueries` for cache
 * refresh.
 */
export const taskApi = $api;
export { fetchClient };

/**
 * Refreshes the job views after an action that changes job state (retry, cancel,
 * delete) or on demand: the listing, the queue summary and the statistics, plus
 * one job's detail when `taskID` is given. All invalidations are started before
 * the first is awaited so they run concurrently, and the promise resolves once
 * every one of them has finished.
 */
export async function invalidateTaskQueries(queryClient: QueryClient, taskID?: string) {
  const invalidations = [
    queryClient.invalidateQueries({ queryKey: ["get", "/v1/jobs"] }),
    queryClient.invalidateQueries({ queryKey: ["get", "/v1/jobs/statistics"] }),
    queryClient.invalidateQueries({ queryKey: ["get", "/v1/jobs/queues"] }),
  ];

  if (taskID) {
    invalidations.push(
      queryClient.invalidateQueries({
        queryKey: ["get", "/v1/jobs/{jobId}", { params: { path: { jobId: taskID } } }],
      }),
    );
  }

  await Promise.all(invalidations);
}

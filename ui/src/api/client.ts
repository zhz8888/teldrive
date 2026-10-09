import createFetchClient from "openapi-fetch";
import createQueryClient from "openapi-react-query";
import { normalizeApiError } from "./errors";
import type { paths } from "./schema";

/**
 * Prefix every request is resolved against. It stays relative and origin-less so
 * the interface always talks to the server that served it: the Vite dev server
 * proxies `/api` to the Go backend, which serves both the API and the bundle in a
 * built deployment.
 */
const API_BASE_URL = "/api";

/**
 * Request wrapper installed on the generated client. A `fetch` rejection (server
 * unreachable, DNS failure) becomes a `network_error` `ApiError`, while an abort
 * is rethrown untouched so a cancelled request is not reported as a failure; a
 * non-2xx response becomes an `ApiError` built from its error envelope, status,
 * `X-Request-ID` and `Retry-After`.
 */
async function fetchWithApiErrors(input: RequestInfo | URL, init?: RequestInit) {
  let response: Response;
  try {
    response = await fetch(input, init);
  } catch (error) {
    if (error instanceof DOMException && error.name === "AbortError") throw error;
    throw normalizeApiError(error);
  }

  if (response.ok) return response;

  let body: unknown;
  try {
    body = await response.clone().json();
  } catch {
    body = undefined;
  }
  throw normalizeApiError(body, response);
}

/**
 * Typed client over the generated OpenAPI paths. Every operation it exposes runs
 * through `fetchWithApiErrors`, so a non-2xx response rejects with an `ApiError`
 * instead of being returned as a result whose `error` field the caller must check;
 * the success value is still the `{ data, response }` pair `unwrap` reads.
 */
export const fetchClient = createFetchClient<paths>({
  baseUrl: API_BASE_URL,
  fetch: fetchWithApiErrors,
  // The contract declares its array query parameters (the file listing's `category` among
  // them) with `explode: false`, which is one comma-joined value, so this has to match: the
  // default repeats the key (`category=a&category=b`) and the server's decoder rejects that
  // as an invalid value. The style is the default `form` the contract leaves implicit;
  // non-array parameters are serialized by the primitive path either way, and the contract
  // has no object-typed query parameter for the array setting to affect.
  querySerializer: {
    array: { style: "form", explode: false },
  },
});

/**
 * TanStack Query bindings for the same operations (`$api.useQuery`,
 * `$api.useMutation`, `$api.queryOptions`). Hooks and imperative cache work such
 * as `invalidateQueries` therefore address one key scheme, which is what lets a
 * mutation invalidate a query a component declared elsewhere.
 */
export const $api = createQueryClient(fetchClient);

/**
 * Escape hatch for requests the generated hooks do not model comfortably: a
 * `Blob` request body, a header outside the contract such as a share password,
 * or a response the caller parses itself. `path` is the versioned path, for
 * example "/v1/uploads/abc", and failures are normalized exactly like the ones
 * from `fetchClient`.
 */
export function apiFetch(path: string, init?: RequestInit) {
  return fetchWithApiErrors(`${API_BASE_URL}${path}`, init);
}

using TypeSpec.Http;
using TypeSpec.OpenAPI;

namespace TelDrive;

/**
 * Auth scheme for the short-lived `ticket` query parameter that browser EventSource
 * clients use.
 */
model EventTicketAuth is ApiKeyAuth<ApiKeyLocation.query, "ticket">;
alias EventStreamAuth = BearerAuth | ExternalApiKeyAuth | EventTicketAuth;

/** Credential that lets an EventSource URL authenticate without headers. */
model EventStreamTicket {
  /** Short-lived credential suitable for a browser EventSource URL. */
  ticket: string;

  /**
   * When the ticket stops authenticating; it is bound to the issuing account and its
   * default lifetime is two minutes.
   */
  expiresAt: utcDateTime;
}

/** Server-sent event stream response; the body stays open until the client disconnects. */
model EventStreamResponse {
  /** Always 200 once the stream is established. */
  @statusCode
  statusCode: 200;

  /** Always `text/event-stream`. */
  @header("Content-Type")
  contentType: "text/event-stream";

  /** Always `no-cache, no-transform`, so proxies do not buffer the stream. */
  @header("Cache-Control")
  cacheControl: "no-cache, no-transform";

  /**
   * The stream itself: events carry an `id` cursor and a JSON envelope, and control
   * events signal a reset.
   */
  @body
  body: bytes;
}

@tag("Events")
@route("/v1/events")
namespace Events {
  /** Create a short-lived browser-compatible event stream credential. */
  @post
  @route("/ticket")
  @operationId("createEventStreamTicket")
  @useAuth(TelDriveAuth)
  op createTicket():
    | JsonResponse<200, EventStreamTicket>
    | ErrorResponse<401>
    | ErrorResponse<503>;

  /** Subscribe to ordered, replayable events for the authenticated user. */
  @get
  @operationId("streamEvents")
  @useAuth(EventStreamAuth)
  op stream(
    /** Resume after the last successfully processed event. */
    @header("Last-Event-Id") lastEventId?: string,

    /** Header-equivalent cursor for clients that cannot set Last-Event-ID. */
    @query after?: int64,

    /** Optional comma-separated event types to include. */
    @query types?: string[],
  ):
    | EventStreamResponse
    | ErrorResponse<401>
    | ErrorResponse<422>
    | ErrorResponse<429>
    | ErrorResponse<503>;
}

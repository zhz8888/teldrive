using TypeSpec.Http;
using TypeSpec.OpenAPI;

namespace TelDrive;

/** Last recorded health of a channel; `unavailable` keeps the channel out of uploads. */
enum ChannelHealth {
  /** Never probed; the state every newly registered channel starts in. */
  unknownState: "unknown",
  /** A health probe recorded the channel as usable; nothing writes this state today. */
  healthy: "healthy",
  /** A health probe recorded partial trouble; nothing writes this state today. */
  degraded: "degraded",
  /**
   * The channel is full or rejected a send; allocation skips it and it cannot be
   * selected again.
   */
  unavailable: "unavailable",
}

/** One Telegram channel registered to the account. */
model ChannelSummary {
  /** Telegram channel ID. */
  id: int64;
  /** Channel title. */
  name: string;
  /** True for the channel new uploads currently target; at most one channel per account. */
  selected: boolean;
  /** Last recorded health of this channel. */
  health: ChannelHealth;
  /** When the channel was registered with TelDrive. */
  createdAt: utcDateTime;
}

/** A channel Telegram reports the account can manage, registered with TelDrive or not. */
model DiscoveredChannel {
  /** Telegram channel ID. */
  id: int64;
  /** Channel title reported by Telegram. */
  name: string;
}
/** Request to create a new Telegram channel and register it. */
model ChannelCreateRequest {
  /** Optional display name. Telegram may choose the final title. */
  @maxLength(120)
  name?: string;

  /**
   * Whether to make the new channel the upload target; omitted or false leaves the
   * current selection unchanged.
   */
  selected?: boolean = true;
}

/** Request to register Telegram bots by token. */
model BotCreateRequest {
  /** Telegram bot tokens. Tokens are never returned by the API. */
  @minItems(1)
  @maxItems(200)
  tokens: string[];
}

/** One registered Telegram bot. */
model BotSummary {
  /** Telegram bot user ID parsed from the token. */
  id: int64;
  /**
   * Bot @username without the leading `@`; omitted until Telegram reports it during
   * provisioning.
   */
  username?: string;
  /**
   * True once provisioning verified the token with Telegram; a pending or failed bot is
   * false and cannot be used for uploads.
   */
  enabled: boolean;
  /** When the token was registered with TelDrive. */
  createdAt: utcDateTime;
}

/** Result of registering a batch of bot tokens. */
model BotCreateResponse {
  /** Bots stored by this request, in the order their tokens were accepted. */
  bots: BotSummary[];
  /**
   * Zero-based indexes into the request's `tokens` that were rejected as malformed or
   * duplicated.
   */
  failedIndexes: int32[];

  /** Present only when existing channels need background bot provisioning. */
  jobId?: string;
}

@tag("Channels")
@route("/v1/channels")
@useAuth(TelDriveAuth)
namespace Channels {
  /** List the account's registered channels, newest first, as a cursor page. */
  @get
  @operationId("listChannels")
  op list(...CursorQuery):
    | JsonResponse<200, CursorPage<ChannelSummary>>
    | ErrorResponse<401>
    | ErrorResponse<422>;

  /** List Telegram channels where the authenticated user can add administrators. */
  @get
  @route("/discover")
  @operationId("discoverChannels")
  op discover():
    | JsonResponse<200, DiscoveredChannel[]>
    | ErrorResponse<401>
    | ErrorResponse<429>;

  /** Discover and upsert manageable Telegram channels without deleting missing rows. */
  @post
  @route("/sync")
  @operationId("syncChannels")
  op sync():
    | JsonResponse<200, ChannelSummary[]>
    | ErrorResponse<401>
    | ErrorResponse<422>
    | ErrorResponse<429>;
  /**
   * Create a Telegram channel named by the request, register it and optionally select
   * it; a blank name gets a generated one.
   */
  @post
  @operationId("createChannel")
  op create(@body body: ChannelCreateRequest):
    | JsonResponse<201, ChannelSummary>
    | ErrorResponse<401>
    | ErrorResponse<409>
    | ErrorResponse<422>
    | ErrorResponse<429>;

  /**
   * Make one registered channel the upload target, clearing the previous selection; an
   * `unavailable` channel is refused with 409.
   */
  @post
  @route("/{channelId}/select")
  @operationId("selectChannel")
  op select(@path channelId: int64):
    | JsonResponse<200, ChannelSummary>
    | ErrorResponse<401>
    | ErrorResponse<404>
    | ErrorResponse<409>;

  /**
   * Delete a registered channel and its Telegram channel; the selected channel and one
   * still holding stored parts are refused with 409.
   */
  @delete
  @route("/{channelId}")
  @operationId("deleteChannel")
  op delete(@path channelId: int64):
    | EmptyResponse<204>
    | ErrorResponse<401>
    | ErrorResponse<404>
    | ErrorResponse<409>;
}

@tag("Bots")
@route("/v1/bots")
@useAuth(BearerOrCookieAuth)
namespace Bots {
  /** List the account's registered bots, newest first, as a cursor page. */
  @get
  @operationId("listBots")
  op list(...CursorQuery):
    | JsonResponse<200, CursorPage<BotSummary>>
    | ErrorResponse<401>
    | ErrorResponse<422>;

  /**
   * Register bot tokens: valid ones are stored disabled and queued for provisioning,
   * malformed or duplicate ones come back in `failedIndexes`.
   */
  @post
  @operationId("createBots")
  op create(@body body: BotCreateRequest):
    | JsonResponse<200, BotCreateResponse>
    | ErrorResponse<400>
    | ErrorResponse<401>
    | ErrorResponse<422>
    | ErrorResponse<429>;
  /** Remove one registered bot by its Telegram bot ID; an unknown or foreign ID answers 404. */
  @delete
  @route("/{botId}")
  @operationId("deleteBot")
  op delete(@path botId: int64):
    | EmptyResponse<204>
    | ErrorResponse<401>
    | ErrorResponse<404>
    | ErrorResponse<409>;
}

using TypeSpec.Http;
using TypeSpec.OpenAPI;

namespace TelDrive;

/**
 * Result of a health probe: the state observed at that moment and the version that
 * answered.
 */
model HealthStatus {
  /**
   * `ok` when the probe passed; `degraded` when a required dependency failed or was
   * never configured.
   */
  status: "ok" | "degraded";
  /**
   * Build version of the answering process; may be empty when the binary carries no
   * version information.
   */
  version: string;
}

@tag("System")
@route("/health")
@useAuth(NoAuth)
namespace System {
  /** Process liveness check. */
  @get
  @route("/live")
  @operationId("healthLive")
  op live(): JsonResponse<200, HealthStatus>;

  /** Readiness check for PostgreSQL, migrations, and required runtime state. */
  @get
  @route("/ready")
  @operationId("healthReady")
  op ready(): JsonResponse<200, HealthStatus> | ErrorResponse<503>;
}

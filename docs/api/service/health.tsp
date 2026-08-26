import "@typespec/http";

using Http;

@tag("Health")
namespace ClubManager.Health;

model HealthResponse {
  @example("ok")
  status: string;
}

/**
 * Liveness probe. The only route that needs no authentication.
 *
 * It reports that the process is serving; it does not check Postgres or Google.
 */
@useAuth(NoAuth)
@summary("Liveness probe")
@route("/healthz")
@get
op healthz(): {
  @statusCode statusCode: 200;
  @body body: HealthResponse;
};

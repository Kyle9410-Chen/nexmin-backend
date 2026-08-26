import "@typespec/http";

using Http;

@tag("Error")
namespace ClubManager.Errors;

/**
 * RFC 9457 problem details, served as `application/problem+json`.
 *
 * Every failure below 500 uses this shape. Note the lower-case field names: the
 * response is produced by `summer/pkg/problem`, whose struct tags are lower-case.
 */
model ProblemDetail {
  /** A short, human-readable summary of the problem. */
  @example("Not Found")
  title: string;

  /** The HTTP status code, repeated in the body. */
  @example(404)
  status: int32;

  /** A URI identifying the problem type. Currently an MDN status-code reference. */
  @example("https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/404")
  type: url;

  /** A human-readable explanation specific to this occurrence. */
  @example("mailing list not found: nosuchgroup")
  detail: string;

  /** The request path this problem occurred on. */
  @example("/api/groups/nosuchgroup/members")
  instance?: string;

  /** Per-field messages, present when request body validation failed. */
  errors?: string[];
}

/** Request body or path parameter failed validation. */
@error
model ValidationProblem {
  @statusCode statusCode: 400;
  @header contentType: "application/problem+json";
  @body body: ProblemDetail;
}

/** The `Authorization` header is missing, malformed, expired, or not an access token. */
@error
model UnauthorizedProblem {
  @statusCode statusCode: 401;
  @header contentType: "application/problem+json";
  @body body: ProblemDetail;
}

/**
 * The caller is authenticated but lacks the `admin` role.
 *
 * The role comes from the access token claim, which is set at sign-in from the
 * caller's role in the login mailing list. Being promoted in Google Groups therefore
 * takes effect on the caller's next sign-in, not immediately.
 */
@error
model ForbiddenProblem {
  @statusCode statusCode: 403;
  @header contentType: "application/problem+json";
  @body body: ProblemDetail;
}

/** The addressed user, mailing list, or member does not exist. */
@error
model NotFoundProblem {
  @statusCode statusCode: 404;
  @header contentType: "application/problem+json";
  @body body: ProblemDetail;
}

/** The address is already a member of this mailing list. */
@error
model ConflictProblem {
  @statusCode statusCode: 409;
  @header contentType: "application/problem+json";
  @body body: ProblemDetail;
}

/** Google's Directory API quota was exceeded. Retry later. */
@error
model TooManyRequestsProblem {
  @statusCode statusCode: 429;
  @header contentType: "application/problem+json";
  @body body: ProblemDetail;
}

/**
 * The Google integration is unavailable.
 *
 * Google credentials are **optional** by design, so this is the expected response on
 * every Google-backed route when no service account key is configured — a local
 * development server returns it, and that is not a fault. It also covers Google being
 * unreachable and domain-wide delegation having been revoked or re-scoped.
 */
@error
model ServiceUnavailableProblem {
  @statusCode statusCode: 503;
  @header contentType: "application/problem+json";
  @body body: ProblemDetail;
}

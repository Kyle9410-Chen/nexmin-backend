import "@typespec/http";
import "./error.tsp";

using Http;

@tag("Auth")
namespace Nexmin.Auth;

/**
 * A fresh token pair.
 *
 * Access tokens are stateless HS256 JWTs and cannot be revoked before they expire —
 * the short lifetime is the mitigation. Refresh tokens are opaque row IDs, which is
 * what makes revocation possible, and they **rotate**: the token presented to
 * `/auth/refresh` is invalidated once its replacement exists.
 */
model TokenResponse {
  /** JWT bearer token, valid for `expiresIn` seconds. */
  @example("eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9...")
  accessToken: string;

  /** Opaque refresh token, valid for 24 hours and single-use. */
  @example("6f1d0f6c-3b1f-4a5a-9f1a-2b0c9d8e7f60")
  refreshToken: uuid;

  /** Lifetime of `accessToken` in seconds. */
  @example(900)
  expiresIn: int64;
}

model RefreshRequest {
  @example("6f1d0f6c-3b1f-4a5a-9f1a-2b0c9d8e7f60")
  refreshToken: uuid;
}

/**
 * Start the Google OAuth flow.
 *
 * Redirects the browser to Google. Nothing is returned as JSON — the result of the
 * whole flow arrives at the callback below.
 */
@useAuth(NoAuth)
@summary("Start Google sign-in")
@route("/auth/google/login")
@get
op googleLogin(
  /** Where the frontend should send the user after a successful sign-in. Carried
   * through the signed OAuth state parameter and echoed back on the callback. */
  @query
  @example("/dashboard")
  redirect?: string,
): {
  /** Redirect to Google's consent screen, or back to the frontend with
   * `#error=login_not_configured` when OAuth or the login group is unset. */
  @statusCode statusCode: 307;

  @header location: url;
};

/**
 * Complete the Google OAuth flow. The browser is sent here by Google, not by the
 * frontend.
 *
 * On success it redirects to the configured frontend URL with the tokens in the
 * **URL fragment**, never the query string, so they stay out of server logs,
 * `Referer` headers and proxy records:
 *
 * ```
 * {frontendURL}#accessToken=<jwt>&refreshToken=<uuid>&expiresIn=900&redirect=<path>
 * ```
 *
 * On failure the fragment is `#error=<reason>` instead, with one of:
 *
 * | reason | meaning |
 * |---|---|
 * | `login_not_configured` | the server has no OAuth client or no login group |
 * | `oauth_denied` | the user declined at Google's consent screen |
 * | `invalid_state` | state parameter missing, tampered with, or older than 5 minutes |
 * | `exchange_failed` | Google rejected the authorization code |
 * | `userinfo_failed` | Google accepted the code but the profile fetch failed |
 * | `email_not_verified` | the Google account's email is not verified |
 * | `membership_check_failed` | the login group could not be read |
 * | `not_a_member` | the address is not a usable member of the login group |
 * | `server_error` | token minting or the user upsert failed |
 *
 * `not_a_member` creates **no** user row. Note that fragments are never sent to a
 * server, so OpenAPI has no way to describe these as response fields.
 */
@useAuth(NoAuth)
@summary("Complete Google sign-in")
@route("/auth/google/callback")
@get
op googleCallback(
  /** Signed state token minted by `/auth/google/login`. */
  @query state?: string,

  /** Authorization code issued by Google. */
  @query code?: string,

  /** Set by Google when the user declined or the request was rejected. */
  @query error?: string,
): {
  /** Redirect to the frontend, with tokens or an error in the fragment. */
  @statusCode statusCode: 307;

  @header location: url;
};

/**
 * Exchange a refresh token for a new pair.
 *
 * The token travels in the body rather than the path so it stays out of access logs
 * and `Referer` headers. It is rotated on every use, so a stolen token is usable at
 * most once before the legitimate client's next refresh invalidates it.
 *
 * This re-reads the user row but **not** the mailing list, so a promotion or demotion
 * made in Google Groups does not reach the token's `role` claim until the user signs
 * in again.
 */
@useAuth(NoAuth)
@summary("Exchange a refresh token")
@route("/auth/refresh")
@post
op refresh(@body body: RefreshRequest):
  | {
      @statusCode statusCode: 200;
      @body body: TokenResponse;
    }
  | Errors.ValidationProblem
  | Errors.UnauthorizedProblem;

/**
 * Revoke every refresh token belonging to the caller.
 *
 * Outstanding access tokens stay valid until they expire; that is inherent to
 * stateless JWTs.
 */
@summary("Revoke all refresh tokens")
@useAuth(BearerAuth)
@route("/auth/logout")
@post
op logout():
  | {
      @statusCode statusCode: 204;
    }
  | Errors.UnauthorizedProblem;

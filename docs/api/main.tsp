import "@typespec/http";
import "@typespec/openapi";
import "@typespec/versioning";
import "./service/error.tsp";
import "./service/health.tsp";
import "./service/auth.tsp";
import "./service/user.tsp";
import "./service/group.tsp";

using Http;
using OpenAPI;
using Versioning;

/**
 * Backend service for NYCU SDC club management.
 *
 * Authority comes from the club's Google Workspace mailing lists, never from local
 * configuration: sign-in is gated on membership of the login group, and the `admin`
 * role is derived from being an OWNER or MANAGER of it. See `docs/organization.md`
 * for how the club's structure maps onto those lists.
 */
@service(#{ title: "SDC Club Manager API" })
@server("http://localhost:8080", "Local development")
@versioned(Versions)
@tagMetadata(
  "Health",
  #{
    description: "Liveness probe. The only unauthenticated route that returns JSON.",
  }
)
@tagMetadata(
  "Auth",
  #{
    description: "Google OAuth sign-in and session tokens. Sign-in is gated on membership of the login mailing list.",
  }
)
@tagMetadata(
  "User",
  #{
    description: "Profiles users maintain themselves, and the mailing lists they are on.",
  }
)
@tagMetadata(
  "Group",
  #{
    description: "Google Workspace mailing lists and their members. Writes require the admin role.",
  }
)
@tagMetadata("Error", #{ description: "Shared RFC 9457 problem shapes." })
@route("/api")
namespace ClubManager;

enum Versions {
  v1: "1.0.0",
}

@format("uuid")
scalar uuid extends string;

@format("email")
scalar email extends string;

/**
 * Every list endpoint returns this shape. There is deliberately no pagination: the
 * club is a few hundred people and every list is bounded by a single mailing list.
 */
model ListResponse<T> {
  items: T[];

  @example(3)
  totalItems: int32;
}

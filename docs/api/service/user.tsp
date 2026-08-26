import "@typespec/http";
import "./error.tsp";
import "./group.tsp";

using Http;

@tag("User")
namespace ClubManager.Users;

/** This service's own roles, as carried in the access token's `role` claim. */
union LocalRole {
  member: "member",
  admin: "admin",
}

model UserResponse {
  id: uuid;

  /** The address the account is keyed on, verified by Google at sign-in. */
  @example("member@example.com")
  email: email;

  /**
   * Display name. Seeded from the Google profile when the account is first created,
   * and owned by the user from then on — a later sign-in never overwrites it.
   */
  @example("王小明")
  name: string;

  @example("小明")
  nickname: string;

  /** Free text; deliberately not validated against a list of departments. */
  @example("資工系")
  department: string;

  /**
   * Derived from the caller's role in the login mailing list on every read, not read
   * straight from the database — the stored column is a cache and is refreshed
   * whenever this endpoint notices it has drifted.
   *
   * Authorization still uses the token's claim, which lags by up to one access token
   * lifetime, so a demoted admin reads as `member` here while briefly retaining
   * access. See `docs/organization.md`.
   */
  role: LocalRole;

  createdAt: utcDateTime;
  updatedAt: utcDateTime;

  /**
   * Mailing lists this user is listed on, in organizational order. Direct membership
   * only; nested lists are not expanded.
   *
   * Populated **only** on `/users/me`; on `/users/{user_id}` it is always `null`,
   * because filling it would cost a Google call per user. The roster endpoint
   * `/users` carries them on `RosterEntry.groups` instead.
   *
   * `null` also means "could not be determined" — Google credentials are optional and
   * the profile must still load without them. That is different from `[]`, which
   * would mean the user is genuinely on no lists. For the full shape, with sections
   * and nesting paths, use `/users/me/groups`.
   */
  @example(#["general", "engineering"])
  groups: string[] | null;
}

/**
 * A partial update: omit a field to leave it unchanged.
 *
 * Sending `""` is a real request and clears the field, which is why omission and
 * emptiness have to be distinguishable. `role` is deliberately absent — it comes from
 * the mailing list or nowhere.
 */
model UpdateProfileRequest {
  /** Trimmed; at most 100 characters. Whitespace-only is rejected, since a blank name
   * would leave the member unidentifiable in every roster that displays it. */
  @example("王小明")
  name?: string;

  /** Trimmed; at most 50 characters. Send `""` to clear. */
  @example("小明")
  nickname?: string;

  /** Trimmed; at most 100 characters. Send `""` to clear. */
  @example("資工系")
  department?: string;
}

/**
 * What this service knows about someone beyond their membership. It exists only once
 * they have signed in at least once.
 */
model RosterProfile {
  id: uuid;

  @example("王小明")
  name: string;

  @example("小明")
  nickname: string;

  @example("資工系")
  department: string;

  createdAt: utcDateTime;
  updatedAt: utcDateTime;
}

/**
 * Adds someone to the club.
 */
model AddRosterMemberRequest {
  @example("member@example.com")
  email: email;

  /**
   * The role on the **login mailing list**, not this service's role. Omit for `MEMBER`,
   * which is Google's own default.
   *
   * `MANAGER` and `OWNER` of the login group map onto this service's `admin`, so
   * setting either **grants administrative access** — the endpoint is named for adding
   * a member, but this field is how admin is handed out.
   */
  role?: Groups.MemberRole;
}

/**
 * One person on the club roster.
 *
 * The roster comes from the login mailing list, not from this service's database: the
 * list decides who exists, and the local row -- when there is one -- only supplies the
 * profile they filled in here.
 */
model RosterEntry {
  @example("member@example.com")
  email: email;

  /** Derived from the caller's role in the login mailing list, live on every read. */
  role: LocalRole;

  /** The mailing lists this person is listed on, in organizational order. Direct
   * membership only; nested lists are not expanded. Always populated -- unlike
   * `UserResponse.groups`, which is only filled on `/users/me`. */
  @example(#["general", "engineering"])
  groups: string[];

  /** Null for someone who is on the mailing list but has never signed in here. */
  profile: RosterProfile | null;
}

/** One mailing list the caller reaches. */
model MembershipItem {
  /** Bare group name, usable directly as `group_key` on the group routes. */
  @example("branding")
  key: string;

  /** Display name from the org chart, falling back to `key` for a list nobody has
   * classified yet. English, with the `NYCU SDC` prefix Google carries stripped. */
  @example("Branding")
  name: string;

  @example(16)
  memberCount: int64;

  /** The officer responsible for this list, `null` for lists no role owns. */
  ownerRole: OfficerRole | null;
}

/**
 * An officer position. Names stay in Chinese -- these are the club's own titles --
 * while the mailing lists around them are named in English.
 */
model OfficerRole {
  @example("engineering_vp")
  key: string;

  @example("開發副社長")
  name: string;
}

model MembershipSection {
  @example("departments")
  key: string;

  @example("Departments")
  name: string;

  items: MembershipItem[];
}

model MyGroupsResponse {
  /** Sections in organizational order. Lists the org chart marks hidden are omitted
   * entirely, and anything unclassified is collected into a trailing `unsectioned`
   * section rather than dropped. */
  sections: MembershipSection[];

  @example(6)
  totalItems: int32;

  /**
   * Whether the caller holds an officer position — but not which one.
   *
   * All six office holders are MANAGER of every department group, so Google cannot
   * tell the positions apart. Anything finer needs a separate source of truth.
   */
  leadership: boolean;
}

/** The caller's own profile. */
@summary("Get the caller's profile")
@useAuth(BearerAuth)
@route("/users/me")
@get
op getMe():
  | {
      @statusCode statusCode: 200;
      @body body: UserResponse;
    }
  | Errors.UnauthorizedProblem
  | Errors.NotFoundProblem;

/** Edit the caller's own name, nickname and department. */
@summary("Update the caller's profile")
@useAuth(BearerAuth)
@route("/users/me")
@patch(#{ implicitOptionality: false })
op updateMe(@body body: UpdateProfileRequest):
  | {
      @statusCode statusCode: 200;
      @body body: UserResponse;
    }
  | Errors.ValidationProblem
  | Errors.UnauthorizedProblem
  | Errors.NotFoundProblem;

/**
 * The mailing lists the caller is listed on, grouped by the club's structure.
 *
 * **Direct membership only.** The club's lists nest -- `consultants` sits inside
 * `committee`, `design` inside `branding` -- and that nesting is deliberately not
 * expanded: doing so would report every consultant as being on the committee, which is
 * true of where their mail lands and false of where they sit in the club. This answers
 * the second question.
 *
 * Everything here comes from Google, so unlike `/users/me` there is nothing to
 * degrade to when the Directory API is unavailable.
 */
@summary("List the caller's mailing lists")
@useAuth(BearerAuth)
@route("/users/me/groups")
@get
op getMyGroups():
  | {
      @statusCode statusCode: 200;
      @body body: MyGroupsResponse;
    }
  | Errors.UnauthorizedProblem
  | Errors.ServiceUnavailableProblem;

/**
 * The club roster: every direct member of the login mailing list, with the lists each
 * of them reaches and, for those who have signed in, their profile.
 *
 * The mailing list is the source of who exists; the `users` table is an extension of
 * it, holding only what this service owns. So an entry with `profile: null` is a real
 * club member who simply has not used this service yet, and the count here is the club,
 * not the set of local accounts.
 *
 * Membership is direct only: someone reachable through a nested group but not listed on
 * the login group itself is absent, exactly as they would be at sign-in.
 *
 * Sorted by the name each person goes by, falling back to their address.
 *
 * Unlike `/users/me`, this has nothing to degrade to when Google is unavailable —
 * without the mailing list there is no roster — so it answers 503 rather than an
 * emptier list.
 */
@summary("List the club roster")
@useAuth(BearerAuth)
@route("/users")
@get
op listUsers():
  | {
      @statusCode statusCode: 200;
      @body body: ListResponse<RosterEntry>;
    }
  | Errors.UnauthorizedProblem
  | Errors.ForbiddenProblem
  | Errors.ServiceUnavailableProblem;

/**
 * Add someone to the club.
 *
 * There is no local account to create: the login mailing list is what decides who
 * exists, so adding someone to it *is* the whole operation. A local row appears by
 * itself the first time they sign in, which is why the entry comes back with
 * `profile: null` for anyone new.
 *
 * The list written to is whichever one `google_group.login_group` names — the same one
 * the sign-in gate reads — so this also decides who can sign in. Callers do not need to
 * know its address.
 *
 * The returned `groups` always contains the login group, even though the Directory API
 * may not report the new membership for a moment yet.
 *
 * 409 means they are already on the list.
 */
@summary("Add someone to the club")
@useAuth(BearerAuth)
@route("/users")
@post
op addUser(@body body: AddRosterMemberRequest):
  | {
      @statusCode statusCode: 201;
      @body body: RosterEntry;
    }
  | Errors.ValidationProblem
  | Errors.UnauthorizedProblem
  | Errors.ForbiddenProblem
  | Errors.NotFoundProblem
  | Errors.ConflictProblem
  | Errors.ServiceUnavailableProblem;

/**
 * Remove someone from the club, by taking them off the login mailing list. They stop
 * appearing on the roster and can no longer sign in; their profile row is left alone,
 * so it comes back if they are added again.
 *
 * Addressed by email rather than the UUID `GET /users/{user_id}` takes, because someone
 * who has never signed in has no local row and therefore no UUID — and they are exactly
 * the people a roster edit is most likely to touch.
 *
 * **Nothing stops an admin removing themselves**, which leaves them unable to sign back
 * in; recovering means being re-added from the Google admin console.
 *
 * Removing an address that is not on the list answers **400**, not 404, and the detail
 * reads `Missing required field: memberKey` — that is Google's own wording for "this
 * member key resolves to nothing", passed through unchanged. 404 here means the login
 * group itself could not be found, which points at `google_group.login_group` being
 * misconfigured.
 */
@summary("Remove someone from the club")
@useAuth(BearerAuth)
@route("/users/{email}")
@delete
op removeUser(@path email: email):
  | {
      @statusCode statusCode: 204;
    }
  | Errors.ValidationProblem
  | Errors.UnauthorizedProblem
  | Errors.ForbiddenProblem
  | Errors.NotFoundProblem
  | Errors.ServiceUnavailableProblem;

/** One user by ID. */
@summary("Get a user by ID")
@useAuth(BearerAuth)
@route("/users/{user_id}")
@get
op getUser(@path user_id: uuid):
  | {
      @statusCode statusCode: 200;
      @body body: UserResponse;
    }
  | Errors.ValidationProblem
  | Errors.UnauthorizedProblem
  | Errors.ForbiddenProblem
  | Errors.NotFoundProblem;

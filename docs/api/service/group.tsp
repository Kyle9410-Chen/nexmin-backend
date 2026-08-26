import "@typespec/http";
import "./error.tsp";

using Http;

@tag("Group")
namespace ClubManager.Groups;

/**
 * A member's role in a Google group. Comparison is case-insensitive on input and an
 * omitted role means `MEMBER`, matching Google's own default for `members.insert`.
 *
 * This is Google's role, not this service's `admin`/`member` — the latter is derived
 * from it for the login group only.
 */
union MemberRole {
  member: "MEMBER",
  manager: "MANAGER",
  owner: "OWNER",
}

/** What this service knows about a member beyond their address. */
model MemberProfile {
  @example("王小明")
  name: string;

  @example("小明")
  nickname: string;

  @example("資工系")
  department: string;
}

model MemberResponse {
  /** Google's immutable numeric member ID. */
  @example("112233445566778899000")
  id: string;

  @example("member@example.com")
  email: email;

  role: MemberRole;

  /** `USER` for a person, `GROUP` for a nested mailing list. */
  @example("USER")
  type: string;

  /**
   * Membership status. **An empty string is normal, not an error**: the Directory API
   * only fills this in for identities inside the Workspace account, so every member
   * from an outside domain — most of the club — reports `""`. Only explicitly named
   * states such as `SUSPENDED` mean anything is wrong.
   */
  @example("ACTIVE")
  status: string;

  /**
   * The member's local profile, or `null` when this address has never signed in here.
   *
   * `null` is meaningfully different from a member who signed in and left the fields
   * blank, which is why the three fields are nested rather than flattened.
   */
  profile: MemberProfile | null;
}

/** Where a mailing list sits in the club's structure. */
model GroupSection {
  @example("project")
  key: string;

  @example("Project Team")
  name: string;
}

model GroupResponse {
  /** Google's immutable group ID, accepted anywhere `group_key` is. */
  @example("03x8ty12abcd")
  id: string;

  /** Bare group name, with the club's own domain stripped. Groups belonging to another
   * domain of the account keep their full address. */
  @example("core-system")
  email: string;

  /** The name set in the Google admin console, prefix and all. */
  @example("NYCU SDC Core System")
  name: string;

  /**
   * The club's own name for this list, from the organization chart. Falls back to the
   * bare group name for a list the chart does not mention.
   */
  @example("Core System")
  displayName: string;

  /**
   * The club's classification. A list the chart does not mention reports the synthetic
   * `unsectioned` section rather than being omitted — a newly created mailing list has
   * to be visible before anyone can classify it.
   */
  section: GroupSection;

  @example("")
  description: string;

  @example(23)
  directMembersCount: int64;

  adminCreated: boolean;
  aliases: string[];
}

model AddMemberRequest {
  @example("member@example.com")
  email: email;

  /** Omit for `MEMBER`. */
  role?: MemberRole;
}

model UpdateMemberRequest {
  role: MemberRole;
}

/**
 * Identifies a mailing list. Three spellings are accepted: the bare name
 * (`general`), the full address (`general@sdc.nycu.club`), or the group's immutable
 * ID. A bare name is resolved against the configured domain.
 */
@doc("Mailing list identifier: bare name, full address, or immutable group ID.")
scalar groupKey extends string;

/**
 * Identifies a member within a list: their email address or their immutable member ID.
 */
@doc("Member identifier: email address or immutable member ID.")
scalar memberKey extends string;

/**
 * Every mailing list in the Workspace account, in organizational order.
 *
 * Covers all domains in the account, not just the club's own. Sorted by the
 * organization chart — All Members, Governance, Departments, Infra Team, Project Team,
 * HPC Team, Training, Program, Events, System — with anything unclassified last, rather
 * than by the alphabetical order Google returns.
 *
 * Unlike `/users/me/groups`, this reports sections the chart marks hidden: that view is
 * a personal profile, while this is the whole account, where omitting a row would read
 * as the group having gone missing.
 */
@summary("List mailing lists")
@useAuth(BearerAuth)
@route("/groups")
@get
op listGroups():
  | {
      @statusCode statusCode: 200;
      @body body: ListResponse<GroupResponse>;
    }
  | Errors.UnauthorizedProblem
  | Errors.ServiceUnavailableProblem;

/**
 * The direct members of one mailing list.
 *
 * **This does not recurse.** A nested group appears as a single entry with
 * `type: "GROUP"`; the people inside it are not listed.
 */
@summary("List a mailing list's members")
@useAuth(BearerAuth)
@route("/groups/{group_key}/members")
@get
op listMembers(@path group_key: groupKey):
  | {
      @statusCode statusCode: 200;
      @body body: ListResponse<MemberResponse>;
    }
  | Errors.ValidationProblem
  | Errors.UnauthorizedProblem
  | Errors.NotFoundProblem
  | Errors.ServiceUnavailableProblem;

/**
 * Add a member to a mailing list. Requires the `admin` role.
 *
 * Adding someone to the login group through this route takes effect immediately: the
 * write drops the member cache and suppresses it briefly, so the next read reflects the
 * change rather than a cached or not-yet-propagated list. Adding them in the Google
 * admin console instead can take up to the cache TTL to allow sign-in.
 */
@summary("Add a member")
@useAuth(BearerAuth)
@route("/groups/{group_key}/members")
@post
op addMember(@path group_key: groupKey, @body body: AddMemberRequest):
  | {
      @statusCode statusCode: 201;
      @body body: MemberResponse;
    }
  | Errors.ValidationProblem
  | Errors.UnauthorizedProblem
  | Errors.ForbiddenProblem
  | Errors.NotFoundProblem
  | Errors.ConflictProblem
  | Errors.TooManyRequestsProblem
  | Errors.ServiceUnavailableProblem;

/** Change an existing member's role. Requires the `admin` role. */
@summary("Change a member's role")
@useAuth(BearerAuth)
@route("/groups/{group_key}/members/{member_key}")
@patch(#{ implicitOptionality: false })
op updateMember(
  @path group_key: groupKey,
  @path member_key: memberKey,
  @body body: UpdateMemberRequest,
):
  | {
      @statusCode statusCode: 200;
      @body body: MemberResponse;
    }
  | Errors.ValidationProblem
  | Errors.UnauthorizedProblem
  | Errors.ForbiddenProblem
  | Errors.NotFoundProblem
  | Errors.TooManyRequestsProblem
  | Errors.ServiceUnavailableProblem;

/**
 * Remove a member from a mailing list. Requires the `admin` role.
 *
 * Removing an address that is not on the list answers **400**, not 404, and the detail
 * reads `Missing required field: memberKey` — Google's own wording for "this member key
 * resolves to nothing", passed through unchanged. 404 here means the group was not
 * found.
 */
@summary("Remove a member")
@useAuth(BearerAuth)
@route("/groups/{group_key}/members/{member_key}")
@delete
op removeMember(@path group_key: groupKey, @path member_key: memberKey):
  | {
      @statusCode statusCode: 204;
    }
  | Errors.ValidationProblem
  | Errors.UnauthorizedProblem
  | Errors.ForbiddenProblem
  | Errors.NotFoundProblem
  | Errors.TooManyRequestsProblem
  | Errors.ServiceUnavailableProblem;

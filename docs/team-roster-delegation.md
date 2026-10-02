# Delegated roster management (the Steward role)

**Status:** DRAFT, 2026-10-01. Design agreed by Stefan; reviewed against the
code on 2026-09-30 and 2026-10-01 (claims marked *verified* or *not yet
tested*). Not yet implemented.
**Scope:** FOKS only, server and client library. Built in the fork first,
then proposed upstream as the separate PRs in §10. Only the pieces marked
*fork-only* stay here. This document is fork-local, because it names SECO
roles, so it does not go into an upstream PR as-is.
**Base:** fork `main` @ `8390b4c`; `upstream/main` @ `438f351`.
**Product source:** `seco-team-knowledge-base`,
`seco-product/blueprints/member-directory-and-roles/MEMBER-DIRECTORY-AND-ROLES.md`
§0.4 (capabilities) and Act 3; `trusted-invitations/TRUSTED-INVITATIONS.md`
3.1.5; `channels/CHANNELS.md` 3.1. Carter put the Steward tier in MVP on
2026-09-29.
**Decision record:** §4.

---

## 1. What this gives

A community gets a third tier between Leader (FOKS admin) and Member, called
**Steward** in SECO. FOKS itself never uses that word. It sees a member at
role `m/100` in a team whose **delegation floor** is `m/100`.

A Steward can, on their own device and without an admin being online:

- add new plain members (role `m/0` or below);
- remove plain members **they added themselves** (the own-invitees check is
  fork-only, §5.8);
- create open channels, rename, edit and archive open channels, and remove
  people from a private channel they are in (§5.7).

A Steward cannot:

- promote or demote anyone, including themselves;
- add, remove or change another Steward, a Leader, an owner, or any member
  above `m/0`. The SECO daemon is a team admin, so it is out of reach too;
- add someone to a private channel;
- read admin-tier content (they hold keys at `m/100` and below, never the
  admin key);
- change the team's delegation floor.

Teams without a floor behave exactly as they do today. That includes every
existing team, DM teams, and every upstream team that never sets one.

## 2. What protects each rule

**Signed** means the rule is part of the team chain: every client re-checks
it on every load, and a link that breaks it makes the team fail to load. The
server cannot forge or waive it. **Encrypted** means the data is unreadable
without a key. **Server only** means the server refuses the request; a
dishonest or buggy server could allow it.

| Rule | Protected by |
|---|---|
| A Steward may sign roster changes only in a team that has a floor, and only while at or above it | Signed |
| A Steward may add only at `m/0` or below | Signed |
| A Steward may remove only members at `m/0` or below | Signed |
| A Steward cannot change roles, key generations, or the floor | Signed |
| Only an admin or owner may set or change the floor | Signed |
| A Steward cannot read admin-tier channels or data | Encrypted: they never receive the admin key |
| A removed member cannot read anything written after removal | Encrypted: the keys at their role and below are rotated |
| A removed member can check that a real team manager removed them | Removal-key MAC; the server cannot produce it |
| A Steward removes only members they added (fork-only) | **Server only** (§5.8) |
| Who may fetch delegated removal-key boxes | Server, and encryption: the boxes open only with the floor key, which only Stewards and above hold |
| Channel actions: create open, rename/edit, archive open, remove from a private channel | **Server only** (§5.7), like every channel permission today |
| Who may see or post in a private channel | **Server only**, as today (`docs/rt-private-channel-acl.md`) |
| The host switch that allows floors at all | **Server only** (§5.6) |

**What a dishonest server could do:**
- let a Steward remove a plain member they did not add;
- let a Steward perform channel actions the policy refuses;
- refuse service.

**What it could not do:**
- let a Steward touch anyone above `m/0`;
- promote anyone;
- read anything;
- make a Steward's link pass on clients that check it.

## 3. Requirements

From the blueprint's capability table (§0.4), restricted to what FOKS
enforces:

| Capability | Leader | Steward | Member | After this change |
|---|---|---|---|---|
| Invite (add) new members | ✓ | ✓ | — | Chain rule (§5.3) |
| Remove a member | ✓ | Only own invitees | — | Chain rule (plain members only) + fork-only server check (§5.8). Stefan, 2026-10-01: Stewards remove only their own invitees. |
| Change another member's role | ✓ | — | — | Chain rule (unchanged: admin or above) |
| Create an open channel | ✓ | ✓ | — | Floor action (§5.7) |
| Create a private channel | ✓ | — | — | Unchanged (admin, `server/realtime/acl.go:322`) |
| Rename or edit a channel | ✓ | ✓ | — | Floor action |
| Archive an open channel | ✓ | ✓ | — | Floor action |
| Archive a private channel | ✓ | — | — | Unchanged (admin) |
| Remove someone from a private channel | ✓ | ✓ | — | Floor action, fork-only |
| Add someone to a private channel | ✓ | If policy allows | If policy allows | Unchanged: channel owner or admin (`acl.go:295`). The per-community policy is not built (§11). |

The Steward acts alone. `TRUSTED-INVITATIONS.md` 3.1.5 says Leaders cannot act
on a Steward's invitations. Any design where a Leader or a Leader's device
must sign each Steward action fails this.

## 4. Options considered

| Option | Why not |
|---|---|
| New role type (`STEWARD` in `RoleType`) | Roles are ranked by type number, so a type between `MEMBER` (1) and `ADMIN` (2) needs every comparison rewritten. Older clients reject unknown role types (`lib/core/role.go:37`). A fork-only enum value would clash with any value upstream adds later. XL. |
| Steward as a client label only | The Steward could not add or remove anyone, because the chain rule requires admin. |
| SECO daemon signs on the Steward's behalf | Not every community has a daemon; it is a paid feature. |
| Signer bot run by the host | The host would hold the admin key of every community and could read everything admins can. This breaks end-to-end encryption. |
| A Leader's client signs automatically | A Leader who is offline for days blocks every Steward, and no one can see why. |
| Member-role team bearer tokens for Stewards | This would break the invariant that a logged-in user's token is an admin token (`server/shared/team_bearer_token.go:630`), which the edit RPC relies on. Stewards post as themselves instead (§5.5). |
| Stewards may add or remove any member below the floor (`m/1`–`m/99`) | Admins use positive levels to hand out extra access (for example "eyes only" channels at `m/5`). A Steward could then grant that access. Limiting Stewards to `m/0` and below keeps those levels admin-only. |
| Re-boxing removal keys for Stewards in a separate step after opt-in | It leaves a window, and a crash can leave it open, in which Stewards cannot remove pre-existing members. Sending the boxes inside the same link closes it (§5.4). |
| "Own invitees" from `social_invites.inviter` | The `invitee` column is "a hint, not proof" (`server/sql/foks_users.sql:1021`): any seed holder can replace a reply. The signer of the link that added the member is exact (§5.8). |
| **Chain rule with a per-team floor (chosen)** | Works in every community, keeps keys on members' own devices, Stewards act immediately. Costs: a chain-rule change, and clients must be updated before Stewards are used (§6). |

## 5. Design

### 5.1 Steward = member at `m/100`

No new role type. `m/100` leaves room for levels below it and above it, for
other roles later. A Steward holds the `m/100` key, so a stewards-only channel
works with read role `m/100`.

### 5.2 The delegation floor

A new link metadata entry, next to `MemberLoadFloor`:

```
enum ChangeType {
    ...
    MemberLoadFloor @5;
    RosterDelegationFloor @6;   // new
}

variant ChangeMetadata switch (t : ChangeType) {
    ...
    case MemberLoadFloor @3 : Role;
    case RosterDelegationFloor @4 : Role;   // new
}
```

- **Who sets it:** an admin or owner (signed, §5.3). It can go in the eldest
  link or any later one, alone or together with roster changes.
- **Values:** a `MEMBER` role with viz level ≥ 1 turns delegation on at that
  level. `NONE` turns it off. Any other value is refused, as is more than one
  entry per link.
- **The floor role must have a key after the link.** That is true if someone
  already holds the role, or if the same link promotes someone to it. Keys are
  created only when a role is first used (`lib/team/core.go:292`). The editor
  therefore offers "promote to the floor and set the floor" as one link. That
  is how the first Steward is made. The server refuses a link that would leave
  the floor without a key.
- **Where it is kept:**
  - The client stores it in team state next to `MemberLoadFloor`
    (`client/libclient/team_loader.go:410`).
  - The server stores it in a new table (SQL patch in `foks_users`), written
    in the transaction that accepts the link. `openLink` reads it under the
    same team lock as the roster (`shared.LoadRoster`,
    `server/shared/team.go:789`), the way `LoadMemberLoadFloor` reads its value
    (`:2180`).
- **CLI:** `foks team change-roles` gains `--delegation-floor <role>`.

### 5.3 The chain rule

The rule is a pure function, `delegatedChangeAllowed(floor, doer, changes,
roster)`, table-tested on its own. `checkChangesLocked`
(`lib/team/core.go:345`) calls it. The floor reaches it through
`GameplanOpts`, from both callers: client replay and editor, and the server's
`openLink` (`server/engine/team_admin.go:571`).

A doer with role `Rd` may sign the change set if:

- `Rd` is admin or owner: **today's rule, unchanged**; or
- all of the following hold:
  1. the team has a floor `F` before this link;
  2. `Rd` is a `MEMBER` role and `F ≤ Rd`;
  3. the doer is on the team's host, matching the rule that only local members
     can be admins (`lib/team/core.go:401`);
  4. every change in the set is one of:
     - **add:** the member is not in the roster, and the new role is `m/0` or
       below;
     - **remove:** the member is in the roster with a role of `m/0` or below,
       and the new role is `NONE`;
  5. no change names the doer;
  6. the link does not carry a `RosterDelegationFloor` entry.

Everything else stays admin-only. That includes:
- role changes and key-generation bumps;
- any change to a member above `m/0`;
- processing a member's `TeamLeaveSelf`, which today is always an admin edit
  (`client/libclient/team_minder_inbox.go:1337`).

The existing checks at `lib/team/core.go:387-396` stay as they are. The new
rule is stricter.

**Concurrency:** the server checks each link against the floor and roster it
loaded under the team lock. If an admin changes the floor, or demotes the
Steward, while a Steward's link is in flight, that link was built on the
previous seqno. The server rejects it, and the Steward's client reloads and
re-evaluates.

### 5.4 Keys for adding and removing

**Admin removal-key box without the admin's private key.** Each added member
gets a removal key, boxed for the member and for the admins
(`lib/team/chain.go:714`). Today the client builds the admin box from the
private admin PTK (`client/libclient/team_minder_edit.go:448`,
`newAdminBoxer`), although it only uses the public half. Build it from
`TeamWrapper.TeamMemberKeys(core.AdminRole)` (`team_loader.go:184`) instead.
That returns the admin PTK's public keys and HEPK to any member (*verified by
reading*). Admins produce the same box as before.

**A third box for the floor role.** Today only admins (`rk_team`) and the
member (`rk_member`) can open a removal key, so a Steward cannot build the
removal MAC (`team_minder_edit.go:565`).

- New nullable column `rk_delegate` (SQL patch on `team_removal_keys`,
  `server/sql/foks_users.sql:250`). It holds the removal key boxed for the
  floor role's PTK, with its role and generation, as `rk_team` has.
- `OffchainBoxData` (`proto-src/rem/team.snowp:118`) gets a trailing field for
  these boxes, so they travel inside `EditTeam` with the link they belong to.
- **Server invariant, checked in the same transaction:** after any accepted
  link in a team with a floor, every member at `m/0` or below has an
  `rk_delegate` box for the current floor role. Links supply the boxes they
  owe:
  - **an add** (by an admin or a Steward) boxes the new member's key;
  - **the link that sets or changes the floor** boxes every existing plain
    member's key. The admin client reads those from the `rk_team` boxes
    through a new admin-only bulk fetch; today there is only a per-member
    fetch (`server/engine/team_admin.go:963`);
  - **a demotion to `m/0`** boxes that member's key.

  Pre-existing members are therefore covered by the opt-in link itself. The
  server stores the link and its boxes in one transaction, so an interrupted
  opt-in leaves either both or neither. There is no window in which a Steward
  cannot remove a pre-existing member.

  **With own-invitees (§5.8), SECO never needs these fill-in boxes for
  Steward removals.** A Steward can remove only members they added, and each
  of their adds boxes its own key. The fill-in exists for upstream, where a
  Steward may remove any plain member. The fork keeps it anyway, because the
  invariant is one rule for every team, and dropping it would make a
  fork-only difference in shared code.
- **Fetch:** a new RPC returns `rk_delegate` to a logged-in caller whose
  current role is at or above the floor, only for members at `m/0` or below,
  and only boxes for the current floor role. Admins keep the existing token
  RPC.
- **Post:** no new RPC. Removals already travel in `EditTeam`'s offchain data
  (`server/engine/team_admin.go:360`, `CheckAndInsertRemovals`).
- **Older key generations:** a Steward promoted after the floor key rotated
  must open boxes made at an older generation. FOKS seals each old seed under
  the next generation and sends the chain to new holders
  (`team_minder_edit.go:337`, `server/shared/team_loader.go:369`). Admins
  already rely on this for `rk_team` (`team_minder_edit.go:594`). *Not yet
  tested*; PR 4 tests it.
- The MAC payload names the remover in a field called `Admin`
  (`team_minder_edit.go:619`). For a Steward it holds the Steward's party.
  The wire name stays as it is.

**Removal rotates keys correctly without admin rights.** It rotates only the
keys at or below the removed member's role (`lib/team/core.go:256`,
`keyFlood`). For an `m/0` member those are `m/0` and the KV-minimum key, which
a Steward holds. The new keys are boxed to every remaining member above, from
public keys in the team chain, admins included. No per-user loads are needed,
even on closed hosts (`team_minder_edit.go:300`, `:371`) (*verified by
reading*).

### 5.5 Stewards post as themselves

Team bearer tokens stay admin-only (§4). A Steward posts `EditTeam` without a
token, as the logged-in signer. That path exists today
(`checkAgainstCurrentParty`, `server/engine/team_admin.go:604`). The chain rule
decides what the link may contain.

- **Client:** `loadTeamAndAdminToken` (`client/libclient/team_minder_inbox.go:104`)
  takes the tokenless path when the caller has no admin key. Today it would
  call `ptk.GetRole()` on a nil key (`:50`). `prepareAllRemovals`
  (`team_minder_edit.go:645`) fetches through the logged-in RPC (§5.4)
  instead of failing without a token. `TeamMinder.Add` and `ChangeRoles`
  (`team_minder_add.go:266`, `team_minder_change.go:91`) use the same branch.
- **Closed hosts:** the add path loads the new member with the team's
  view-only token. The server accepts it when the invitee's grant is at or
  below the token holder's role (`server/shared/user_loader.go:309`).
  Invitees grant at the member load floor, `m/0` by default
  (`server/shared/team.go:1613`), so a Steward passes (*inferred*; PR 3's
  closed-host integration test covers it).
- **Unchanged and admin-only:**
  - team certs, the join-request inbox, remote joins, and the team's own
    membership chain (`server/engine/team_admin.go:891`, `:915`, `:936`,
    `:1036`, `:1057`). Because remote joins go through the admin inbox,
    Stewards add only local users in practice;
  - team size limits (`checkTeamLimits`), which apply to Steward links as to
    any other.

### 5.6 Server config

In the existing `team` section of `foks.jsonnet`
(`server/shared/config_jsonnet.go:208`; there is no `team` block in the
shipped configs yet):

```jsonnet
team : {
    // Allows links that set a delegation floor. Default false.
    roster_delegation : true,

    // Role names, passed to clients by GetTeamConfig; FOKS never checks them.
    role_labels : {
        steward : "m/100",
    },

    // In a team with a floor, these actions require at least the floor
    // (admins always pass). Teams without a floor keep the built-in rule.
    floor_actions : [
        "rt.channel.create_open",
        "rt.channel.edit",
        "rt.channel.archive_open",
        "rt.channel.revoke_private",
    ],
},
```

- `roster_delegation` is the one switch: turn it on for staging when ready.
- `GetTeamConfig` (`server/engine/team_admin.go:1070`) returns `role_labels`
  and `floor_actions` as new trailing fields of `TeamConfig`. Older clients
  ignore them (tested, §6). Clients use them for display only; the server
  enforces.
- **Why the floor and not a fixed role:** the config applies to every team on
  the host. A fixed `m/100` would stop plain members of a DM team from
  creating the DM's channel. Tying the actions to the team's own floor
  changes only teams that opted in.
- **More roles later:** a new label at another level needs no FOKS code or
  migration. A second delegating tier would need the floor to become a list.
  That is out of scope.

### 5.7 Floor actions in realtime

A pure resolver answers "may this role do this action in a team with this
floor?" and is table-tested. Each action is registered with its built-in
default in the file that owns the feature. A fork registers its own actions in
its own files, so the list causes no merge conflicts.

| Action | Built-in rule (today) | Checked in | Upstream |
|---|---|---|---|
| `rt.channel.create_open` | any member | `checkPerms` (`server/realtime/channels.go:453`) | Yes |
| `rt.channel.edit` | admin | `authorizeChannel`, gate 4b (`acl.go:250`), for `UpdateChannel` (`channels.go:1377`) | After #376 |
| `rt.channel.archive_open` | admin | gate 4b, for `SetChannelArchived` (`channels.go:1438`) on an open channel | After #376 |
| `rt.channel.revoke_private` | channel owner or admin | `acl.go:295`, for `RevokeChannelMember` (`acl.go:761`) | Fork-only |

- **Gate 4b** stops being a fixed admin check. `authorizeChannel` takes the
  action, and the channel's privacy is already loaded at that point
  (`acl.go:164-208`). Private archive, private creation (`acl.go:322`) and
  grant on a private channel (`acl.go:673`) keep their fixed admin or owner
  rule; they are not floor actions.
- **A Steward outside a private channel** still gets "not found" from the
  chokepoint (`acl.go:214`); only admins bypass a missing ACL row. So a
  Steward removes people only from private channels they belong to.
- **Default channel:** the server cannot read channel names (sealed,
  `channels.go:1333`), so it cannot exempt `#general` from `create_open`. A
  client that creates its default channel lazily, on first send, must create
  it before setting the floor. Otherwise a plain member who sends first is
  refused.
- **Upstream status:** rename and archive are proposed upstream as #376, not
  merged. Private channels are fork-only (`SECO-UPSTREAM.md`).

### 5.8 Own invitees (fork-only)

Decided by Stefan on 2026-10-01: a Steward removes only members they added
themselves.

- **Check:** when the server accepts a link signed by a non-admin, each
  removed member's add is looked up. `team_removal_keys` holds the seqno at
  which the member was added (`create_seqno`). A member added more than once
  has several rows; the one that counts is the current membership's, the row
  with no removal recorded yet. The `links` table holds that link
  (`server/sql/foks_users.sql:69`). The server opens it and compares its
  signer with the Steward. If they differ, it refuses with a
  `PermissionError`, an existing status code, so no fork-only error number is
  needed.
- **Where:** one call in `openLink` (`server/engine/team_admin.go:571`), with
  the function in a fork-only file. The upstream file differs by one line.
- **Why the signer and not the social invite:** see §4. The signer is exact,
  needs no new table, and covers members added before this change.
- **Limits:**
  - A member a Leader added can be removed only by an admin, even after they
    have left with `TeamLeaveSelf` and a Steward is re-inviting them.
  - A member a Steward added and a Leader later re-added belongs to the
    Leader.
  - This is a server check (§2).

### 5.9 Errors

No new status codes. Refusals use the existing `TeamRosterError` (chain rule)
and `PermissionError` (server checks), with messages that name the rule, for
example "delegated roster change may only add members at m/0 or below".

## 6. Older clients and rollout

**What breaks:** a client built before §5.3 replays a Steward-signed link,
refuses it with "doer doesn't have privileged role", and fails to load **that
whole team** (`CLOpenLinkError`, `client/libclient/team_loader.go:1045`). Its
other teams keep working. An older admin client also cannot add members to a
team with a floor, because it does not send delegate boxes and the server's
invariant refuses the link (§5.4).

**What does not break:**

- Teams without a floor: nothing changes.
- The link that only sets the floor: older clients skip the unknown metadata
  type. Non-eldest links have a `switch` with no `default` (`lib/team/chain.go:309-325`).
  Tested 2026-09-30 on a single entry: the decoder reads a `ChangeMetadata`
  with unknown type `T=9` without error. PR 2 adds a test on a full signed
  link.
- The extra `TeamConfig` fields: tested 2026-09-30, decoded without error.
- A member at `m/100` seen by an older client is an ordinary member.

**Rollout:** there is no production server yet.

- **Staging:** deploy with `roster_delegation : false`, then turn it on when
  ready.
- **Pre-production task:** before turning the switch on in production,
  clients that cannot replay Steward-signed links must be forced to update.
  FOKS's own minimum client version only warns (`client/libclient/nag.go`).

**Turning it off:** `roster_delegation : false` stops new floors, but not
existing ones. An admin stops Stewards in a team by setting the floor to
`NONE`. Links already signed by Stewards remain valid history.

## 7. Security review checklist

Each item is a test in §8. Each check also gets a mutation test: remove the
check and confirm a test fails.

- **A Steward cannot:**
  - add at `m/1` or above;
  - change any role;
  - remove a member above `m/0`, another Steward, an admin or an owner;
  - bump a key generation;
  - set or clear the floor;
  - act in a team without a floor, or from another host;
  - remove a member they did not add (fork).
- **A member below the floor** cannot sign any roster change.
- **Floor changes:** lowering, raising or clearing the floor applies from the
  next link, including to a Steward link already in flight.
- **Floor without a key:** a link that sets the floor but leaves the floor role
  without a key is refused. So is any link that leaves a plain member without
  a delegate box.
- **After a Steward's removal,** the removed member cannot open the new `m/0`
  or KV-minimum keys, and every remaining member, admins included, can.
- **Delegate boxes:** the server hands `rk_delegate` only to callers at or
  above the current floor, only for members at `m/0` or below, and never
  `rk_team` to a non-admin.
- **Tokens:** a logged-in user still cannot mint a non-admin team bearer token.
- **Floor actions:**
  - a team without a floor, including a DM team, behaves exactly as today;
  - a Steward cannot grant anyone into a private channel, and cannot see or
    revoke in a private channel they are not in.

## 8. Test plan

- **Unit, `lib/team`:** table-driven `delegatedChangeAllowed` for every line of
  §5.3 and §7. Also: the floor round-trips through replay and cached team
  state.
- **Unit, compatibility:**
  - today's replay opens a full signed link carrying an unknown metadata type;
  - a `TeamConfig` and an `OffchainBoxData` with extra trailing fields decode.
- **Unit, realtime:** the floor-action resolver table, with and without a
  floor.
- **Integration (`integration-tests/lib`), on an open and a closed host:**
  - an admin promotes the first Steward and sets the floor in one link;
  - the Steward adds a user, who can read the team;
  - the Steward removes that user, who cannot read new messages, while the
    admin still can;
  - the Steward removes a member who joined before the floor was set;
  - a Steward promoted after a rotation of the floor key can remove;
  - the Steward is refused for a member a Leader added (fork).
- **Server gates:** drive the raw RPCs as a Steward and as a plain member, not
  only through the client: `EditTeam` without a token, the delegate-box fetch,
  and the admin-only bulk fetch. A client-API test can pass while the server
  check is missing.
- **Must not change:** existing team and realtime tests for teams without a
  floor, the realtime ACL inventory tests, and gate 4b's error text for
  admin-only cases.

## 9. Code structure

- **`lib/team`:**
  - `delegatedChangeAllowed` and the floor value type live in a new file
    `delegation.go`. They are pure, and `checkChangesLocked` calls them;
  - the floor travels in `GameplanOpts`, not in global state;
  - parsing `RosterDelegationFloor` sits next to the `MemberLoadFloor`
    handling in `chain.go`.
- **`client/libclient`:**
  - one helper decides "admin token or logged-in signer" for an edit; the add,
    change and removal paths call it instead of each checking the key;
  - `newAdminBoxer` becomes a function of public keys only.
- **`server`:**
  - floor storage sits next to the `MemberLoadFloor` functions in
    `server/shared/team.go`;
  - the delegate-box invariant is one function called from the same place as
    `CheckAndInsertRemovals`.
- **`server/realtime`:** the floor-action resolver is a small pure function.
  The team's floor is read once per request and passed in.
- **Fork-only:** `revoke_private` registration and the own-invitees check each
  live in their own file. The upstream files differ by one registration line
  and one call line.

## 10. Implementation plan

Branch each upstream PR from `upstream/main`, stacked where noted.

| # | PR | Main files | Size | Upstream |
|---|---|---|---|---|
| 1 | Admin removal-key box from the admin public key | `client/libclient/team_minder_edit.go` | S | Yes |
| 2 | Delegation floor: metadata, client state, server table, switch, compatibility tests | `proto-src/lib/chains.snowp`, `lib/team/chain.go`, `client/libclient/team_loader.go`, `server/shared/team.go`, `server/sql/foks_users.sql` + patch, `server/shared/config_jsonnet.go` | M | Yes |
| 3 | Chain rule, tokenless edit path, CLI flag (on 1, 2) | `lib/team/delegation.go`, `lib/team/core.go`, `server/engine/team_admin.go`, `client/libclient/team_minder_*.go` | M–L | Yes |
| 4 | Delegate removal boxes: column, offchain field, invariant, bulk and delegate fetch (on 3) | SQL patch, `proto-src/rem/team.snowp`, `server/engine/team_admin.go`, `server/shared/team.go`, `client/libclient/team_minder_edit.go` | M | Yes |
| 5 | Floor actions, `role_labels`/`floor_actions` in `GetTeamConfig`, `create_open` (on 2) | `server/shared/config*.go`, `server/realtime/*`, `proto-src/rem/team.snowp` | M | Yes |
| 5b | `edit`, `archive_open` (on 5 and #376) | `server/realtime/acl.go`, `channels.go` | S | Yes, after #376 |
| F1 | `revoke_private` (on 5) | `server/realtime` | S | Fork-only |
| F2 | Own-invitees check (on 3) | `server/engine` | S | Fork-only |

- **Numbering:** `ChangeType @6` and `ChangeMetadata @4` are free on
  `upstream/main`. Propose PR 2 early, so that upstream does not use them
  first. If it does, renumber in the fork before any production team has a
  floor. The two SQL patches (PR 2, PR 4) will get different numbers in the
  fork and upstream, because the fork has its own patches (social invites,
  private channels). Reconcile them by content at the next upstream merge, as
  for push holds.
- **Tracker:** add each PR to `SECO-UPSTREAM.md` as `Proposed` when it opens,
  and F1/F2 as `Local` with the reason.

## 11. Open questions

- **Not this change:** the per-community "who may add to a private channel"
  policy is not enforced by the server, which allows the
  channel owner or an admin. It is worth its own change.
- **Follow-up:** should a Steward be allowed to process the `TeamLeaveSelf` of
  a member they added? It would need the server or chain to read the member's
  own membership chain, which has not been checked.

-- Roster delegation: the same removal key, boxed a third time for the
-- delegation floor role, so members at or above the floor can remove this
-- member without the admin PTK. The role columns duplicate the box's
-- EncKey role so the server can check coverage in SQL. NULL until the team
-- opts in (and for teams that never do).
ALTER TABLE team_removal_keys ADD COLUMN rk_delegate BYTEA;
ALTER TABLE team_removal_keys ADD COLUMN rk_delegate_role_type SMALLINT;
ALTER TABLE team_removal_keys ADD COLUMN rk_delegate_viz_level SMALLINT;

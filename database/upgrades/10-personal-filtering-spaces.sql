-- v10: Add personal filtering space info

ALTER TABLE "user" ADD COLUMN space_room TEXT NOT NULL DEFAULT '';
ALTER TABLE portal ADD COLUMN in_space BOOLEAN NOT NULL DEFAULT false;

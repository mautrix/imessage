-- v12: Store user management room ID
ALTER TABLE "user" ADD COLUMN management_room TEXT NOT NULL DEFAULT '';

-- The application stores the display name; uniqueness is case-insensitive
-- through the normalized (lowercased) name below.
CREATE TABLE categories (
    id   BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name TEXT        NOT NULL,
    CONSTRAINT categories_name_not_empty CHECK (name <> ''),
    CONSTRAINT categories_name_length CHECK (char_length(name) <= 120)
);

CREATE UNIQUE INDEX categories_name_lower_idx ON categories (lower(name));

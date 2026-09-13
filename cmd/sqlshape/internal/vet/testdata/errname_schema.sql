-- sqlshape: postgres 17
CREATE TABLE widgets (
    id  bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    qty integer NOT NULL
);

-- sqlshape: error P0501 = TooManyWidgets
CREATE FUNCTION widgets_check() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.qty > 100 THEN
    RAISE EXCEPTION 'too many widgets' USING ERRCODE = 'P0501';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER widgets_check_trigger BEFORE INSERT ON widgets FOR EACH ROW EXECUTE FUNCTION widgets_check();

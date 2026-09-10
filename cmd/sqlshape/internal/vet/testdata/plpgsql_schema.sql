-- sqlshape: postgres 17
CREATE TABLE accounts (
    id      bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner   text NOT NULL,
    balance bigint NOT NULL DEFAULT 0 CHECK (balance >= 0)
);

-- the RAISE inside supplies the SQLSTATE; the annotation only names it
-- sqlshape: error AC001 = Overdrawn
CREATE FUNCTION withdraw(p_id bigint, p_amount bigint) RETURNS bigint LANGUAGE plpgsql AS $$
DECLARE cur bigint;
BEGIN
  SELECT balance INTO cur FROM accounts WHERE id = p_id FOR UPDATE;
  IF cur < p_amount THEN
    RAISE EXCEPTION 'overdrawn' USING ERRCODE = 'AC001';
  END IF;
  UPDATE accounts SET balance = balance - p_amount WHERE id = p_id;
  RETURN cur - p_amount;
END $$;

-- a trigger whose RAISE is not annotated: the code alone is the failure mode
CREATE FUNCTION no_negative() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.balance < 0 THEN RAISE EXCEPTION 'negative balance'; END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER accounts_no_negative BEFORE INSERT OR UPDATE ON accounts FOR EACH ROW EXECUTE FUNCTION no_negative();

-- a body with a type error: reported once as a schema problem
CREATE FUNCTION broken() RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  UPDATE accounts SET balanse = 0;
END $$;

-- EXECUTE of a string built at run time cannot be checked (-strict says so)
CREATE FUNCTION purge(tbl text) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  EXECUTE 'DELETE FROM ' || quote_ident(tbl);
END $$;

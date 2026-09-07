package sqlshape_test

// Adversarial-testing lane "param" (see
// /tmp/claude-1000/-home-kr9ly-projects-sqlshape/c120b088-d858-42eb-b453-2c47d8ce2ea3/scratchpad/adv/lane-param.md).
//
// internal/vet's TestAdvParam (internal/vet/adv_param_test.go) shows that the checker
// warns "int64 into smallint may overflow" for a *scalar* int64 parameter bound to a
// smallint column, but says nothing at all for the same int64 values passed through a
// []int64 parameter bound to a smallint[] column (or []float64 into real[]): the array
// branch of matchValue recurses into matchValue directly instead of going through
// paramFit, which is where the overflow/precision notes live.
//
// This file proves that gap is not just a missing advisory: real PostgreSQL rejects (or
// silently narrows) exactly the values vet let through unremarked.

import (
	"context"
	"testing"
	"time"

	"github.com/kr9ly/sqlshape"
	"github.com/kr9ly/sqlshape/internal/oracle"
)

const advParamSchema = `
CREATE TABLE t (
    id     bigint PRIMARY KEY,
    tags   smallint[],
    reals  real[]
);
`

// TestAdvParamArrayOverflow: a []int64 parameter feeding a smallint[] column vets clean
// (see internal/vet's TestAdvParam, package adv_param, var updateArray) yet a value
// outside smallint range makes PostgreSQL reject the statement at execution time with a
// "smallint out of range" error (SQLSTATE 22003) -- exactly the failure mode paramFit's
// "may overflow" note exists to warn about for the scalar case, silently missing here.
func TestAdvParamArrayOverflow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	o, err := oracle.Start(ctx, advParamSchema)
	if err != nil {
		t.Fatalf("oracle: %v", err)
	}
	defer o.Close()
	db := o.Conn()

	type Params struct {
		ID   int64
		Tags []int64
	}
	insertTags := sqlshape.Query[struct{}, Params](`INSERT INTO t (id, tags) VALUES ({{.ID}}, {{.Tags}})`)

	// 40000 does not fit in a smallint (max 32767): vet raised no diagnostic for this
	// parameter (unlike the scalar equivalent), so nothing told the author this value
	// is unsafe before it reached PostgreSQL.
	_, err = insertTags.Exec(ctx, db, Params{ID: 1, Tags: []int64{40000}})
	if err == nil {
		t.Fatalf("want a PostgreSQL error for a smallint[] element out of int16 range, got none")
	}
	t.Logf("PostgreSQL rejected the unwarned-about array parameter: %v", err)
}

// TestAdvParamArrayPrecisionLoss: a []float64 parameter feeding a real[] (float4[])
// column vets clean (see updateArrayFloat in the same vet testdata package) yet the
// value that comes back from PostgreSQL is not the value that was sent: float4 cannot
// represent it exactly, so the round trip silently changes the number. The scalar
// equivalent (float64 into a real column) gets paramFit's "loses precision" note; the
// array element does not.
func TestAdvParamArrayPrecisionLoss(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	o, err := oracle.Start(ctx, advParamSchema)
	if err != nil {
		t.Fatalf("oracle: %v", err)
	}
	defer o.Close()
	db := o.Conn()

	type Params struct {
		ID    int64
		Reals []float64
	}
	insertReals := sqlshape.Query[struct{}, Params](`INSERT INTO t (id, reals) VALUES ({{.ID}}, {{.Reals}})`)

	const sent = 1.0000001192092896 // exactly representable as float32, but its
	// float64 neighbour below is not, and is what a real Go computation is more
	// likely to produce
	const notSent = 1.0000001192092897
	_, err = insertReals.Exec(ctx, db, Params{ID: 1, Reals: []float64{notSent}})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	type Row struct {
		Reals []float64
	}
	readReals := sqlshape.Query[Row, struct{ ID int64 }](`SELECT reals FROM t WHERE id = {{.ID}}`)
	got, err := readReals.First(ctx, db, struct{ ID int64 }{1})
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(got.Reals) != 1 {
		t.Fatalf("want 1 element, got %v", got.Reals)
	}
	if got.Reals[0] == notSent {
		t.Fatalf("expected the real[] round trip to change the value (float4 cannot hold it exactly), but got the exact value back: %v", got.Reals[0])
	}
	if got.Reals[0] != sent {
		// still demonstrates the point (the value changed silently), just with a
		// different neighbour than expected -- log it rather than fail so the
		// core finding (silent value change, no vet warning) still stands.
		t.Logf("value changed on the real[] round trip as expected, though not to the exact neighbour predicted: sent %v (as float64), got back %v", notSent, got.Reals[0])
	}
}

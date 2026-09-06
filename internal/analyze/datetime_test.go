package analyze

import (
	"testing"
	"time"
)

func TestDatetimeSmoke(t *testing.T) {
	sess := &dtSession{dateOrder: "mdy", tz: time.UTC}
	code := func(e *Error) string {
		if e == nil {
			return "ok"
		}
		return e.Code
	}
	cases := []struct{ kind, in, want string }{
		{"ts", "2001-02-03 04:05:06", "ok"}, {"ts", "epoch", "ok"}, {"ts", "infinity", "ok"}, {"ts", "-infinity", "ok"},
		{"ts", "1999-13-01", "22008"}, {"ts", "garbage", "22007"}, {"ts", "2001-02-30", "22008"}, {"ts", "294277-01-01", "22008"},
		{"ts", "Feb 29 2001", "22008"}, {"ts", "Feb 29 2000", "ok"}, {"ts", "now", "ok"}, {"ts", "tomorrow", "ok"},
		{"ts", "1997-02-07 15:23:27 PST", "ok"}, {"ts", "Mon Feb 10 17:32:01 1997 PST", "ok"}, {"ts", "19970210 173201", "ok"},
		{"ts", "1997.038 15:23:27", "ok"}, {"ts", "20011225T040506.789-07", "ok"}, {"ts", "J2451187", "ok"},
		{"ts", "1999-01-08 04:05:06 America/New_York", "ok"}, {"ts", "1999-01-08 04:05:06 Mars/Olympus", "22023"},
		{"ts", "4714-11-24 00:00:00 BC", "ok"}, {"ts", "4714-11-23 00:00:00 BC", "22008"}, {"ts", "10:00", "22007"},
		{"ts", "1999-01-08 25:00:00", "22008"}, {"ts", "1999-01-08 24:00:00", "ok"}, {"ts", "1999-01-08 +16:00", "22009"},
		{"tstz", "2001-02-03 04:05:06+05:30", "ok"}, {"tstz", "294276-12-31 23:59:59 UTC", "ok"}, {"tstz", "294277-01-01 00:00:00 UTC", "22008"},
		{"date", "01/08/1999", "ok"}, {"date", "13/08/1999", "22008"}, {"date", "1999-01-08 04:05:06", "ok"}, {"date", "J2451187", "ok"},
		{"date", "epoch", "ok"}, {"date", "5874897-12-31", "ok"}, {"date", "5874898-01-01", "22008"}, {"date", "12:00", "22007"},
		{"date", "1999-1-32", "22008"}, {"date", "January 8, 1999", "ok"}, {"date", "1999-Jan-08", "ok"}, {"date", "08-Jan-1999", "ok"},
		{"time", "04:05:06 PST", "ok"}, {"time", "25:00:00", "22008"}, {"time", "24:00:00", "ok"}, {"time", "garbage", "22007"},
		{"time", "12:00 AM", "ok"}, {"time", "13:00 PM", "22008"}, {"time", "040506", "ok"}, {"time", "2003-04-12 04:05:06 America/New_York", "ok"},
		{"time", "T040506", "ok"}, {"time", "allballs", "ok"}, {"time", "04:05:06.789-08", "ok"},
		{"timetz", "04:05:06 America/New_York", "22007"}, {"timetz", "04:05:06 PST", "ok"},
		{"iv", "1 day", "ok"}, {"iv", "@ 1 hour ago", "ok"}, {"iv", "P1Y2M3DT4H5M6S", "ok"}, {"iv", "1-2", "ok"}, {"iv", "1 1:00:00", "ok"},
		{"iv", "garbage", "22007"}, {"iv", "2147483648 days", "22015"}, {"iv", "1 year 1 year", "22007"}, {"iv", "3 hours ago 1 day", "22007"},
		{"iv", "infinity", "ok"}, {"iv", "-infinity", "ok"}, {"iv", "1.5 weeks", "ok"}, {"iv", "P0002-06-07T01:30:00", "ok"}, {"iv", "P20020607T013000", "ok"},
		{"iv", "1 day -1:00:00", "ok"}, {"iv", "-1 -1:00:00", "ok"}, {"iv", "9223372036854775807 microseconds", "ok"}, {"iv", "9223372036854775808 microseconds", "22015"},
		{"iv", "1 millennium", "ok"}, {"iv", "1 millenniums", "ok"}, {"iv", "1 microsecondsX", "ok"}, {"iv", "P1", "ok"}, {"iv", "PT", "ok"},
		{"iv", "1 day ago ago", "22007"}, {"iv", "1 1", "22007"}, {"iv", "1:2:3:4", "22007"}, {"iv", "1 year 2 mons 3 days 04:05:06.789", "ok"},
		{"iv", "178956970 years", "ok"}, {"iv", "178956971 years", "22008"},
	}
	for _, c := range cases {
		var got string
		switch c.kind {
		case "ts":
			got = code(validateTimestampLiteral(c.in, false, sess, 0))
		case "tstz":
			got = code(validateTimestampLiteral(c.in, true, sess, 0))
		case "date":
			got = code(validateDateLiteral(c.in, sess, 0))
		case "time":
			got = code(validateTimeLiteral(c.in, false, sess, 0))
		case "timetz":
			got = code(validateTimeLiteral(c.in, true, sess, 0))
		case "iv":
			got = code(validateIntervalLiteral(c.in, -1, sess, 0))
		}
		if got != c.want {
			t.Errorf("%s %q: got %s want %s", c.kind, c.in, got, c.want)
		}
	}
}

package analyze

// A port of the input side of PG's datetime.c (ParseDateTime, DecodeDateTime,
// DecodeTimeOnly, DecodeInterval, DecodeISO8601Interval) and the range checks the
// input functions apply afterwards (timestamp_in, date_in, time_in, interval_in).
// Only the outcome matters here: accepted, or which SQLSTATE PG would raise. The code
// stays close to the C so it can be diffed against a new PG release.

import (
	"math"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // named zones without relying on the host's zoneinfo
)

const (
	dtErrBadFormat        = -1
	dtErrFieldOverflow    = -2
	dtErrMDFieldOverflow  = -3
	dtErrIntervalOverflow = -4
	dtErrTZDispOverflow   = -5
	dtErrBadTimezone      = -6
	dtErrBadZoneAbbrev    = -7
)

const (
	maxDateLen    = 128
	maxDateFields = 25

	monthsPerYear = 12
	daysPerMonth  = 30
	hoursPerDay   = 24
	minsPerHour   = 60
	secsPerMinute = 60
	secsPerHour   = 3600
	usecsPerDay   = int64(86400000000)
	usecsPerHour  = int64(3600000000)
	usecsPerMin   = int64(60000000)
	usecsPerSec   = int64(1000000)

	maxTZDispHour = 15

	julianMinYear  = -4713
	julianMinMonth = 11
	julianMaxYear  = 5874898
	julianMaxMonth = 6

	postgresEpochJDate = 2451545
	dateEndJulian      = 2147483494
	minTimestamp       = int64(-211813488000000000)
	endTimestamp       = int64(9223371331200000000)

	intervalFullRange = 0x7FFF
	intervalFullPrec  = 0xFFFF
	maxIntervalPrec   = 6
)

// interval range bits (INTERVAL_MASK uses the field type codes)
var (
	imYear   = dtkM(ftYear)
	imMonth  = dtkM(ftMonth)
	imDay    = dtkM(ftDay)
	imHour   = dtkM(ftHour)
	imMinute = dtkM(ftMinute)
	imSecond = dtkM(ftSecond)
)

type pgTM struct {
	year, mon, mday, hour, min, sec, yday, wday, isdst int
}

// itmIn is pg_itm_in: the accumulator DecodeInterval fills.
type itmIn struct {
	usec            int64
	mday, mon, year int
}

// dtSession is the GUC state datetime input depends on.
type dtSession struct {
	dateOrder   string // "mdy" / "dmy" / "ymd"
	sqlStandard bool   // IntervalStyle = sql_standard
	tz          *time.Location
}

type dtExtra struct {
	timezone string
}

// --- character classes (C locale, unsigned char) -----------------------------------

func cIsDigit(c byte) bool { return c >= '0' && c <= '9' }
func cIsAlpha(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
func cIsAlnum(c byte) bool { return cIsDigit(c) || cIsAlpha(c) }
func cIsSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\v' || c == '\f' || c == '\r'
}
func cIsPunct(c byte) bool {
	return (c >= 33 && c <= 47) || (c >= 58 && c <= 64) || (c >= 91 && c <= 96) || (c >= 123 && c <= 126)
}
func cLower(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 32
	}
	return c
}

// strtoNum is strtol / strtoll: optional whitespace and sign, then digits. rest is the
// unparsed tail; when no digits follow, rest is the whole input and val is 0. erange is
// set when the value does not fit bits (the value is then clamped, as strtol does).
func strtoNum(s string, bits int) (val int64, rest string, erange bool) {
	i := 0
	for i < len(s) && cIsSpace(s[i]) {
		i++
	}
	neg := false
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		neg = s[i] == '-'
		i++
	}
	start := i
	var mag uint64
	over := false
	for i < len(s) && cIsDigit(s[i]) {
		d := uint64(s[i] - '0')
		if mag > (math.MaxUint64-d)/10 {
			over = true
		} else {
			mag = mag*10 + d
		}
		i++
	}
	if i == start {
		return 0, s, false
	}
	rest = s[i:]
	var lim uint64 = math.MaxInt64
	var min int64 = math.MinInt64
	if bits == 32 {
		lim, min = math.MaxInt32, math.MinInt32
	}
	if neg {
		if over || mag > lim+1 {
			return min, rest, true
		}
		return -int64(mag), rest, false
	}
	if over || mag > lim {
		return int64(lim), rest, true
	}
	return int64(mag), rest, false
}

// atoi is C atoi: no error reporting.
func atoi(s string) int {
	v, _, _ := strtoNum(s, 32)
	return int(v)
}

// strtod parses a C strtod prefix: [sign] digits [. digits] [e [sign] digits] | inf | nan.
func strtod(s string) (val float64, rest string, ok bool) {
	i := 0
	for i < len(s) && cIsSpace(s[i]) {
		i++
	}
	start := i
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	low := strings.ToLower(s[i:])
	for _, w := range []string{"infinity", "inf", "nan"} {
		if strings.HasPrefix(low, w) {
			f, err := strconv.ParseFloat(s[start:i+len(w)], 64)
			if err != nil {
				return 0, s, false
			}
			return f, s[i+len(w):], true
		}
	}
	digits := 0
	for i < len(s) && cIsDigit(s[i]) {
		i++
		digits++
	}
	if i < len(s) && s[i] == '.' {
		i++
		for i < len(s) && cIsDigit(s[i]) {
			i++
			digits++
		}
	}
	if digits == 0 {
		return 0, s, false
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		j := i + 1
		if j < len(s) && (s[j] == '+' || s[j] == '-') {
			j++
		}
		if j < len(s) && cIsDigit(s[j]) {
			for j < len(s) && cIsDigit(s[j]) {
				j++
			}
			i = j
		}
	}
	f, err := strconv.ParseFloat(s[start:i], 64)
	if err != nil {
		// out of range: strtod sets ERANGE and returns ±HUGE_VAL / 0; callers treat errno != 0 as failure
		return f, s[i:], false
	}
	if f == 0 && strings.ContainsAny(strings.FieldsFunc(s[start:i], func(r rune) bool { return r == 'e' || r == 'E' })[0], "123456789") {
		return 0, s[i:], false // underflow to zero: ERANGE as well
	}
	return f, s[i:], true
}

// --- calendar ------------------------------------------------------------------------

func date2j(y, m, d int) int {
	if m > 2 {
		m++
		y += 4800
	} else {
		m += 13
		y += 4799
	}
	century := y / 100
	julian := y*365 - 32167
	julian += y/4 - century + century/4
	julian += 7834*m/256 + d
	return julian
}

func j2date(jd int) (year, month, day int) {
	julian := uint32(int32(jd))
	julian += 32044
	quad := julian / 146097
	extra := (julian-quad*146097)*4 + 3
	julian += 60 + quad*3 + extra/146097
	quad = julian / 1461
	julian -= quad * 1461
	y := int(julian * 4 / 1461)
	if y != 0 {
		julian = (julian+305)%365 + 123
	} else {
		julian = (julian+306)%366 + 123
	}
	y += int(quad * 4)
	year = y - 4800
	quad = julian * 2141 / 65536
	day = int(julian - 7834*quad/256)
	month = int((quad+10)%monthsPerYear + 1)
	return
}

func isLeap(y int) bool { return y%4 == 0 && (y%100 != 0 || y%400 == 0) }

var dayTab = [2][13]int{
	{31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31, 0},
	{31, 29, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31, 0},
}

func isValidJulian(y, m, _ int) bool {
	return (y > julianMinYear || (y == julianMinYear && m >= julianMinMonth)) &&
		(y < julianMaxYear || (y == julianMaxYear && m < julianMaxMonth))
}

func timeOverflows(hour, min, sec int, fsec int64) bool {
	if hour < 0 || hour > hoursPerDay || min < 0 || min >= minsPerHour ||
		sec < 0 || sec > secsPerMinute || fsec < 0 || fsec > usecsPerSec {
		return true
	}
	return (int64((hour*minsPerHour+min)*secsPerMinute+sec)*usecsPerSec)+fsec > usecsPerDay
}

func dt2time(t int64) (hour, min, sec int, fsec int64) {
	hour = int(t / usecsPerHour)
	t -= int64(hour) * usecsPerHour
	min = int(t / usecsPerMin)
	t -= int64(min) * usecsPerMin
	sec = int(t / usecsPerSec)
	fsec = t - int64(sec)*usecsPerSec
	return
}

// --- overflow-checked accumulation (pg_add_s64_overflow & co) ----------------------

func add64(a, b int64) (int64, bool) {
	c := a + b
	return c, (a >= 0) == (b >= 0) && (c >= 0) != (a >= 0)
}

func mul64(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, false
	}
	c := a * b
	return c, c/b != a || (a == -1 && b == math.MinInt64) || (b == -1 && a == math.MinInt64)
}

func add32(a, b int) (int, bool) {
	c := int64(a) + int64(b)
	return int(c), c > math.MaxInt32 || c < math.MinInt32
}

func mul32(a, b int) (int, bool) {
	c := int64(a) * int64(b)
	return int(c), c > math.MaxInt32 || c < math.MinInt32
}

func int64MultiplyAdd(val, multiplier int64, sum *int64) bool {
	p, over := mul64(val, multiplier)
	if over {
		return false
	}
	s, over := add64(*sum, p)
	if over {
		return false
	}
	*sum = s
	return true
}

func adjustFractMicroseconds(frac float64, scale int64, in *itmIn) bool {
	if frac == 0 {
		return true
	}
	frac *= float64(scale)
	usec := int64(frac)
	frac -= float64(usec)
	if frac > 0.5 {
		usec++
	} else if frac < -0.5 {
		usec--
	}
	s, over := add64(in.usec, usec)
	if over {
		return false
	}
	in.usec = s
	return true
}

func adjustFractDays(frac float64, scale int, in *itmIn) bool {
	if frac == 0 {
		return true
	}
	frac *= float64(scale)
	extra := int(frac)
	d, over := add32(in.mday, extra)
	if over {
		return false
	}
	in.mday = d
	frac -= float64(extra)
	return adjustFractMicroseconds(frac, usecsPerDay, in)
}

func adjustFractYears(frac float64, scale int, in *itmIn) bool {
	extra := int(math.RoundToEven(frac * float64(scale) * monthsPerYear))
	m, over := add32(in.mon, extra)
	if over {
		return false
	}
	in.mon = m
	return true
}

func adjustMicroseconds(val int64, fval float64, scale int64, in *itmIn) bool {
	if !int64MultiplyAdd(val, scale, &in.usec) {
		return false
	}
	return adjustFractMicroseconds(fval, scale, in)
}

func adjustDays(val int64, scale int, in *itmIn) bool {
	if val < math.MinInt32 || val > math.MaxInt32 {
		return false
	}
	days, over := mul32(int(val), scale)
	if over {
		return false
	}
	d, over := add32(in.mday, days)
	if over {
		return false
	}
	in.mday = d
	return true
}

func adjustMonths(val int64, in *itmIn) bool {
	if val < math.MinInt32 || val > math.MaxInt32 {
		return false
	}
	m, over := add32(in.mon, int(val))
	if over {
		return false
	}
	in.mon = m
	return true
}

func adjustYears(val int64, scale int, in *itmIn) bool {
	if val < math.MinInt32 || val > math.MaxInt32 {
		return false
	}
	years, over := mul32(int(val), scale)
	if over {
		return false
	}
	y, over := add32(in.year, years)
	if over {
		return false
	}
	in.year = y
	return true
}

// parseFraction parses ".ddd" to end of string.
func parseFraction(cp string) (float64, int) {
	if len(cp) == 1 {
		return 0, 0
	}
	f, rest, ok := strtod(cp)
	if !ok || rest != "" {
		return 0, dtErrBadFormat
	}
	return f, 0
}

func parseFractionalSecond(cp string) (int64, int) {
	f, dterr := parseFraction(cp)
	if dterr != 0 {
		return 0, dterr
	}
	return int64(math.RoundToEven(f * 1000000)), 0
}

// --- ParseDateTime -------------------------------------------------------------------

// parseDateTime breaks the input into typed fields. buflen is the C workbuf size; a
// longer input is a format error there too.
func parseDateTime(s string, buflen int) (fields []string, ftypes []int, dterr int) {
	used := 0
	i, n := 0, len(s)
	for i < n {
		if cIsSpace(s[i]) {
			i++
			continue
		}
		if len(fields) >= maxDateFields {
			return nil, nil, dtErrBadFormat
		}
		var f []byte
		var ft int
		switch c := s[i]; {
		case cIsDigit(c):
			f = append(f, c)
			i++
			for i < n && cIsDigit(s[i]) {
				f = append(f, s[i])
				i++
			}
			switch {
			case i < n && s[i] == ':':
				ft = dtkTime
				f = append(f, s[i])
				i++
				for i < n && (cIsDigit(s[i]) || s[i] == ':' || s[i] == '.') {
					f = append(f, s[i])
					i++
				}
			case i < n && (s[i] == '-' || s[i] == '/' || s[i] == '.'):
				delim := s[i]
				f = append(f, s[i])
				i++
				if i < n && cIsDigit(s[i]) {
					ft = dtkDate
					if delim == '.' {
						ft = dtkNumber
					}
					for i < n && cIsDigit(s[i]) {
						f = append(f, s[i])
						i++
					}
					if i < n && s[i] == delim {
						ft = dtkDate
						f = append(f, s[i])
						i++
						for i < n && (cIsDigit(s[i]) || s[i] == delim) {
							f = append(f, s[i])
							i++
						}
					}
				} else {
					ft = dtkDate
					for i < n && (cIsAlnum(s[i]) || s[i] == delim) {
						f = append(f, cLower(s[i]))
						i++
					}
				}
			default:
				ft = dtkNumber
			}
		case c == '.':
			f = append(f, c)
			i++
			for i < n && cIsDigit(s[i]) {
				f = append(f, s[i])
				i++
			}
			ft = dtkNumber
		case cIsAlpha(c):
			ft = dtkString
			f = append(f, cLower(c))
			i++
			for i < n && cIsAlpha(s[i]) {
				f = append(f, cLower(s[i]))
				i++
			}
			isDate := false
			if i < n && (s[i] == '-' || s[i] == '/' || s[i] == '.') {
				isDate = true
			} else if i < n && (s[i] == '+' || cIsDigit(s[i])) {
				if _, ok := lookupTok(datetktbl, string(f)); !ok {
					isDate = true
				}
			}
			if isDate {
				ft = dtkDate
				for {
					f = append(f, cLower(s[i]))
					i++
					if !(i < n && (s[i] == '+' || s[i] == '-' || s[i] == '/' || s[i] == '_' ||
						s[i] == '.' || s[i] == ':' || cIsAlnum(s[i]))) {
						break
					}
				}
			}
		case c == '+' || c == '-':
			f = append(f, c)
			i++
			for i < n && cIsSpace(s[i]) {
				i++
			}
			switch {
			case i < n && cIsDigit(s[i]):
				ft = dtkTZ
				f = append(f, s[i])
				i++
				for i < n && (cIsDigit(s[i]) || s[i] == ':' || s[i] == '.' || s[i] == '-') {
					f = append(f, s[i])
					i++
				}
			case i < n && cIsAlpha(s[i]):
				ft = dtkSpecial
				f = append(f, cLower(s[i]))
				i++
				for i < n && cIsAlpha(s[i]) {
					f = append(f, cLower(s[i]))
					i++
				}
			default:
				return nil, nil, dtErrBadFormat
			}
		case cIsPunct(c):
			i++
			continue
		default:
			return nil, nil, dtErrBadFormat
		}
		used += len(f) + 1
		if used > buflen {
			return nil, nil, dtErrBadFormat
		}
		fields = append(fields, string(f))
		ftypes = append(ftypes, ft)
	}
	return fields, ftypes, 0
}

// --- helpers shared by the decoders ---------------------------------------------------

func decodeSpecial(lowtoken string) (typ, val int) {
	t, ok := lookupTok(datetktbl, lowtoken)
	if !ok {
		return ftUnknown, 0
	}
	return t.typ, t.value
}

func decodeUnits(lowtoken string) (typ, val int) {
	t, ok := lookupTok(deltatktbl, lowtoken)
	if !ok {
		return ftUnknown, 0
	}
	return t.typ, t.value
}

// decodeTimezoneAbbrev: TZ / DTZ with offset, DYNTZ with its zone, or ftUnknown.
func decodeTimezoneAbbrev(lowtoken string, extra *dtExtra) (typ, offset int, tz *time.Location, dterr int) {
	key := lowtoken
	if len(key) > tokMaxLen {
		key = key[:tokMaxLen]
	}
	a, ok := tzAbbrevs[key]
	if !ok {
		return ftUnknown, 0, nil, 0
	}
	if a.typ == ftDynTZ {
		loc := pgTzSet(a.zone)
		if loc == nil {
			extra.timezone = a.zone
			return a.typ, 0, nil, dtErrBadZoneAbbrev
		}
		return a.typ, 0, loc, 0
	}
	return a.typ, a.offset, nil, 0
}

// decodeTimezone reads "+hh", "+hh:mm", "+hh:mm:ss", "+hhmm". The result follows PG's
// sign convention (positive west of Greenwich).
func decodeTimezone(str string) (tzp int, dterr int) {
	if str == "" || (str[0] != '+' && str[0] != '-') {
		return 0, dtErrBadFormat
	}
	hr64, cp, erange := strtoNum(str[1:], 32)
	if erange {
		return 0, dtErrTZDispOverflow
	}
	hr := int(hr64)
	min, sec := 0, 0
	if cp != "" && cp[0] == ':' {
		m, cp2, erange := strtoNum(cp[1:], 32)
		if erange {
			return 0, dtErrTZDispOverflow
		}
		min, cp = int(m), cp2
		if cp != "" && cp[0] == ':' {
			s, cp2, erange := strtoNum(cp[1:], 32)
			if erange {
				return 0, dtErrTZDispOverflow
			}
			sec, cp = int(s), cp2
		}
	} else if cp == "" && len(str) > 3 {
		min = hr % 100
		hr = hr / 100
	}
	if hr < 0 || hr > maxTZDispHour || min < 0 || min >= minsPerHour || sec < 0 || sec >= secsPerMinute {
		return 0, dtErrTZDispOverflow
	}
	tz := (hr*minsPerHour+min)*secsPerMinute + sec
	if str[0] == '-' {
		tz = -tz
	}
	if cp != "" {
		return -tz, dtErrBadFormat
	}
	return -tz, 0
}

// pgTzSet resolves a zone name the way pg_tzset does: a tzdb name (case-insensitively),
// else a POSIX TZ string such as "EST5EDT" or "GMT+8". nil when unknown.
func pgTzSet(name string) *time.Location {
	if name == "" || len(name) > 255 || strings.Contains(name, "..") || strings.HasPrefix(name, "/") {
		return nil
	}
	if loc, err := time.LoadLocation(name); err == nil && name != "Local" {
		return loc
	}
	// case-insensitive retry: UTC / GMT / EST5EDT style names are upper case, the rest title case per word
	if up := strings.ToUpper(name); up != name {
		if loc, err := time.LoadLocation(up); err == nil {
			return loc
		}
	}
	segs := strings.Split(name, "/")
	for i, seg := range segs {
		var b strings.Builder
		start := true
		for j := 0; j < len(seg); j++ {
			c := seg[j]
			switch {
			case start && cIsAlpha(c):
				b.WriteByte(c &^ 0x20)
			default:
				b.WriteByte(cLower(c))
			}
			start = c == '_' || c == '-'
		}
		segs[i] = b.String()
	}
	if cand := strings.Join(segs, "/"); cand != name {
		if loc, err := time.LoadLocation(cand); err == nil {
			return loc
		}
	}
	return posixTZ(name)
}

// posixTZ accepts std offset [dst [offset] [,rule]] and returns a fixed zone for std.
func posixTZ(s string) *time.Location {
	name, rest, ok := posixZoneName(s)
	if !ok {
		return nil
	}
	off, rest, ok := posixOffset(rest)
	if !ok {
		return nil
	}
	if rest != "" {
		if _, rest, ok = posixZoneName(rest); !ok {
			return nil
		}
		if rest != "" && rest[0] != ',' {
			if _, rest, ok = posixOffset(rest); !ok {
				return nil
			}
		}
		if rest != "" && rest[0] != ',' {
			return nil
		}
	}
	return time.FixedZone(name, -off)
}

func posixZoneName(s string) (name, rest string, ok bool) {
	if strings.HasPrefix(s, "<") {
		end := strings.IndexByte(s, '>')
		if end < 0 {
			return "", s, false
		}
		return s[1:end], s[end+1:], true
	}
	i := 0
	for i < len(s) && cIsAlpha(s[i]) {
		i++
	}
	if i < 3 {
		return "", s, false
	}
	return s[:i], s[i:], true
}

// posixOffset parses [+-]hh[:mm[:ss]] (seconds, POSIX sign: positive west).
func posixOffset(s string) (off int, rest string, ok bool) {
	i := 0
	neg := false
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		neg = s[i] == '-'
		i++
	}
	num := func() (int, bool) {
		st := i
		for i < len(s) && cIsDigit(s[i]) {
			i++
		}
		if st == i || i-st > 2 {
			return 0, false
		}
		return atoi(s[st:i]), true
	}
	h, ok := num()
	if !ok || h > 24 {
		return 0, s, false
	}
	off = h * secsPerHour
	if i < len(s) && s[i] == ':' {
		i++
		m, ok := num()
		if !ok || m > 59 {
			return 0, s, false
		}
		off += m * secsPerMinute
		if i < len(s) && s[i] == ':' {
			i++
			sec, ok := num()
			if !ok || sec > 59 {
				return 0, s, false
			}
			off += sec
		}
	}
	if neg {
		off = -off
	}
	return off, s[i:], true
}

// determineTimeZoneOffset is the PG-signed offset of the zone at the local time in tm.
func determineTimeZoneOffset(tm *pgTM, loc *time.Location) int {
	if loc == nil {
		return 0
	}
	t := time.Date(tm.year, time.Month(tm.mon), tm.mday, tm.hour, tm.min, tm.sec, 0, loc)
	_, off := t.Zone()
	tm.isdst = 0
	if t.IsDST() {
		tm.isdst = 1
	}
	return -off
}

func currentTM(loc *time.Location) pgTM {
	now := time.Now().In(loc)
	return pgTM{year: now.Year(), mon: int(now.Month()), mday: now.Day(), hour: now.Hour(), min: now.Minute(), sec: now.Second(), isdst: -1}
}

// --- DecodeDate / ValidateDate / DecodeTime / DecodeNumber(Field) ------------------

func decodeDate(str string, fmask int, tmask *int, is2digits *bool, tm *pgTM, sess *dtSession) int {
	*tmask = 0
	var fields []string
	i := 0
	for i < len(str) && len(fields) < maxDateFields {
		for i < len(str) && !cIsAlnum(str[i]) {
			i++
		}
		if i >= len(str) {
			return dtErrBadFormat
		}
		st := i
		if cIsDigit(str[i]) {
			for i < len(str) && cIsDigit(str[i]) {
				i++
			}
		} else if cIsAlpha(str[i]) {
			for i < len(str) && cIsAlpha(str[i]) {
				i++
			}
		}
		fields = append(fields, str[st:i])
		if i < len(str) {
			i++ // drop the delimiter
		}
	}
	haveTextMonth := false
	done := make([]bool, len(fields))
	for i, f := range fields {
		if !cIsAlpha(f[0]) {
			continue
		}
		typ, val := decodeSpecial(f)
		if typ == ftIgnore {
			continue
		}
		dmask := dtkM(typ)
		if typ != ftMonth {
			return dtErrBadFormat
		}
		tm.mon = val
		haveTextMonth = true
		if fmask&dmask != 0 {
			return dtErrBadFormat
		}
		fmask |= dmask
		*tmask |= dmask
		done[i] = true
	}
	for i, f := range fields {
		if done[i] {
			continue
		}
		if len(f) == 0 {
			return dtErrBadFormat
		}
		var dmask int
		var fsec int64
		if dterr := decodeNumber(len(f), f, haveTextMonth, fmask, &dmask, tm, &fsec, is2digits, sess); dterr != 0 {
			return dterr
		}
		if fmask&dmask != 0 {
			return dtErrBadFormat
		}
		fmask |= dmask
		*tmask |= dmask
	}
	if fmask&^(dtkM(ftDOY)|dtkM(ftTZ)) != dtkDateM {
		return dtErrBadFormat
	}
	return 0
}

func validateDate(fmask int, isjulian, is2digits, bc bool, tm *pgTM) int {
	if fmask&dtkM(ftYear) != 0 {
		switch {
		case isjulian:
		case bc:
			if tm.year <= 0 {
				return dtErrFieldOverflow
			}
			tm.year = -(tm.year - 1)
		case is2digits:
			if tm.year < 0 {
				return dtErrFieldOverflow
			}
			if tm.year < 70 {
				tm.year += 2000
			} else if tm.year < 100 {
				tm.year += 1900
			}
		default:
			if tm.year <= 0 {
				return dtErrFieldOverflow
			}
		}
	}
	if fmask&dtkM(ftDOY) != 0 {
		tm.year, tm.mon, tm.mday = j2date(date2j(tm.year, 1, 1) + tm.yday - 1)
	}
	if fmask&dtkM(ftMonth) != 0 && (tm.mon < 1 || tm.mon > monthsPerYear) {
		return dtErrMDFieldOverflow
	}
	if fmask&dtkM(ftDay) != 0 && (tm.mday < 1 || tm.mday > 31) {
		return dtErrMDFieldOverflow
	}
	if fmask&dtkDateM == dtkDateM {
		leap := 0
		if isLeap(tm.year) {
			leap = 1
		}
		if tm.mday > dayTab[leap][tm.mon-1] {
			return dtErrFieldOverflow
		}
	}
	return 0
}

// itm is pg_itm for DecodeTimeCommon: hour may exceed int32 in intervals.
type itm struct {
	hour     int64
	min, sec int
	usec     int64
}

func decodeTimeCommon(str string, rng int, tmask *int, it *itm) int {
	*tmask = dtkTimeM
	var cp string
	var erange bool
	it.hour, cp, erange = strtoNum(str, 64)
	if erange {
		return dtErrFieldOverflow
	}
	if cp == "" || cp[0] != ':' {
		return dtErrBadFormat
	}
	m, cp, erange := strtoNum(cp[1:], 32)
	if erange {
		return dtErrFieldOverflow
	}
	it.min = int(m)
	var fsec int64
	switch {
	case cp == "":
		it.sec = 0
		if rng == imMinute|imSecond {
			if it.hour > math.MaxInt32 || it.hour < math.MinInt32 {
				return dtErrFieldOverflow
			}
			it.sec = it.min
			it.min = int(it.hour)
			it.hour = 0
		}
	case cp[0] == '.':
		var dterr int
		if fsec, dterr = parseFractionalSecond(cp); dterr != 0 {
			return dterr
		}
		if it.hour > math.MaxInt32 || it.hour < math.MinInt32 {
			return dtErrFieldOverflow
		}
		it.sec = it.min
		it.min = int(it.hour)
		it.hour = 0
	case cp[0] == ':':
		s, cp2, erange := strtoNum(cp[1:], 32)
		if erange {
			return dtErrFieldOverflow
		}
		it.sec = int(s)
		cp = cp2
		if cp != "" && cp[0] == '.' {
			var dterr int
			if fsec, dterr = parseFractionalSecond(cp); dterr != 0 {
				return dterr
			}
		} else if cp != "" {
			return dtErrBadFormat
		}
	default:
		return dtErrBadFormat
	}
	if it.hour < 0 || it.min < 0 || it.min > minsPerHour-1 || it.sec < 0 || it.sec > secsPerMinute ||
		fsec < 0 || fsec > usecsPerSec {
		return dtErrFieldOverflow
	}
	it.usec = fsec
	return 0
}

func decodeTime(str string, rng int, tmask *int, tm *pgTM, fsec *int64) int {
	var it itm
	if dterr := decodeTimeCommon(str, rng, tmask, &it); dterr != 0 {
		return dterr
	}
	if it.hour > math.MaxInt32 {
		return dtErrFieldOverflow
	}
	tm.hour, tm.min, tm.sec, *fsec = int(it.hour), it.min, it.sec, it.usec
	return 0
}

func decodeTimeForInterval(str string, rng int, tmask *int, in *itmIn) int {
	var it itm
	if dterr := decodeTimeCommon(str, rng, tmask, &it); dterr != 0 {
		return dterr
	}
	in.usec = it.usec
	if !int64MultiplyAdd(it.hour, usecsPerHour, &in.usec) ||
		!int64MultiplyAdd(int64(it.min), usecsPerMin, &in.usec) ||
		!int64MultiplyAdd(int64(it.sec), usecsPerSec, &in.usec) {
		return dtErrFieldOverflow
	}
	return 0
}

func decodeNumber(flen int, str string, haveTextMonth bool, fmask int, tmask *int, tm *pgTM, fsec *int64, is2digits *bool, sess *dtSession) int {
	*tmask = 0
	v, cp, erange := strtoNum(str, 32)
	if erange {
		return dtErrFieldOverflow
	}
	if cp == str {
		return dtErrBadFormat
	}
	val := int(v)
	if cp != "" && cp[0] == '.' {
		if len(str)-len(cp) > 2 {
			if dterr := decodeNumberField(flen, str, fmask|dtkDateM, tmask, tm, fsec, is2digits); dterr < 0 {
				return dterr
			}
			return 0
		}
		f, dterr := parseFractionalSecond(cp)
		if dterr != 0 {
			return dterr
		}
		*fsec = f
	} else if cp != "" {
		return dtErrBadFormat
	}
	if flen == 3 && fmask&dtkDateM == dtkM(ftYear) && val >= 1 && val <= 366 {
		*tmask = dtkM(ftDOY) | dtkM(ftMonth) | dtkM(ftDay)
		tm.yday = val
		return 0
	}
	switch fmask & dtkDateM {
	case 0:
		switch {
		case flen >= 3 || sess.dateOrder == "ymd":
			*tmask = dtkM(ftYear)
			tm.year = val
		case sess.dateOrder == "dmy":
			*tmask = dtkM(ftDay)
			tm.mday = val
		default:
			*tmask = dtkM(ftMonth)
			tm.mon = val
		}
	case dtkM(ftYear):
		*tmask = dtkM(ftMonth)
		tm.mon = val
	case dtkM(ftMonth):
		if haveTextMonth {
			if flen >= 3 || sess.dateOrder == "ymd" {
				*tmask = dtkM(ftYear)
				tm.year = val
			} else {
				*tmask = dtkM(ftDay)
				tm.mday = val
			}
		} else {
			*tmask = dtkM(ftDay)
			tm.mday = val
		}
	case dtkM(ftYear) | dtkM(ftMonth):
		if haveTextMonth {
			if flen >= 3 && *is2digits {
				*tmask = dtkM(ftDay)
				tm.mday = tm.year
				tm.year = val
				*is2digits = false
			} else {
				*tmask = dtkM(ftDay)
				tm.mday = val
			}
		} else {
			*tmask = dtkM(ftDay)
			tm.mday = val
		}
	case dtkM(ftDay):
		*tmask = dtkM(ftMonth)
		tm.mon = val
	case dtkM(ftMonth) | dtkM(ftDay):
		*tmask = dtkM(ftYear)
		tm.year = val
	case dtkM(ftYear) | dtkM(ftMonth) | dtkM(ftDay):
		if dterr := decodeNumberField(flen, str, fmask, tmask, tm, fsec, is2digits); dterr < 0 {
			return dterr
		}
		return 0
	default:
		return dtErrBadFormat
	}
	if *tmask == dtkM(ftYear) {
		*is2digits = flen <= 2
	}
	return 0
}

// decodeNumberField reads a run-together date or time; returns a DTK token or a DTERR.
func decodeNumberField(length int, str string, fmask int, tmask *int, tm *pgTM, fsec *int64, is2digits *bool) int {
	if dot := strings.IndexByte(str, '.'); dot >= 0 {
		cp := str[dot:]
		if len(cp) == 1 {
			*fsec = 0
		} else {
			f, _, ok := strtod(cp)
			if !ok {
				return dtErrBadFormat
			}
			*fsec = int64(math.RoundToEven(f * 1000000))
		}
		str = str[:dot]
		length = len(str)
	} else if fmask&dtkDateM != dtkDateM {
		if length >= 6 {
			*tmask = dtkDateM
			tm.mday = atoi(str[length-2:])
			tm.mon = atoi(str[length-4 : length-2])
			tm.year = atoi(str[:length-4])
			if length-4 == 2 {
				*is2digits = true
			}
			return dtkDate
		}
	}
	if fmask&dtkTimeM != dtkTimeM {
		switch length {
		case 6:
			*tmask = dtkTimeM
			tm.sec = atoi(str[4:])
			tm.min = atoi(str[2:4])
			tm.hour = atoi(str[:2])
			return dtkTime
		case 4:
			*tmask = dtkTimeM
			tm.sec = 0
			tm.min = atoi(str[2:])
			tm.hour = atoi(str[:2])
			return dtkTime
		}
	}
	return dtErrBadFormat
}

// --- DecodeDateTime -------------------------------------------------------------------

// decodeDateTime interprets the fields as a date and time. It returns 0 for a full
// date, 1 for a time only (callers treat that as an error), or a DTERR code.
func decodeDateTime(fields []string, ftypes []int, sess *dtSession, extra *dtExtra) (dtype int, tm pgTM, fsec int64, tzp int, ret int) {
	fmask := 0
	ptype := 0
	mer := merHR24
	haveTextMonth, isjulian, is2digits, bc := false, false, false, false
	var namedTz, abbrevTz *time.Location
	dtype = dtkDate
	tm.isdst = -1

	for i := range fields {
		field := fields[i]
		tmask := 0
		switch ftypes[i] {
		case dtkDate:
			if ptype == dtkJulian {
				jd, cp, erange := strtoNum(field, 32)
				if erange || jd < 0 {
					return dtype, tm, fsec, tzp, dtErrFieldOverflow
				}
				tm.year, tm.mon, tm.mday = j2date(int(jd))
				isjulian = true
				var dterr int
				if tzp, dterr = decodeTimezone(cp); dterr != 0 {
					return dtype, tm, fsec, tzp, dterr
				}
				tmask = dtkDateM | dtkTimeM | dtkM(ftTZ)
				ptype = 0
				break
			}
			if ptype != 0 || fmask&(dtkM(ftMonth)|dtkM(ftDay)) == dtkM(ftMonth)|dtkM(ftDay) {
				if cIsDigit(field[0]) || ptype != 0 {
					if ptype != 0 {
						if ptype != dtkTime {
							return dtype, tm, fsec, tzp, dtErrBadFormat
						}
						ptype = 0
					}
					if fmask&dtkTimeM == dtkTimeM {
						return dtype, tm, fsec, tzp, dtErrBadFormat
					}
					dash := strings.IndexByte(field, '-')
					if dash < 0 {
						return dtype, tm, fsec, tzp, dtErrBadFormat
					}
					var dterr int
					if tzp, dterr = decodeTimezone(field[dash:]); dterr != 0 {
						return dtype, tm, fsec, tzp, dterr
					}
					head := field[:dash]
					if dterr = decodeNumberField(len(head), head, fmask, &tmask, &tm, &fsec, &is2digits); dterr < 0 {
						return dtype, tm, fsec, tzp, dterr
					}
					tmask |= dtkM(ftTZ)
				} else {
					namedTz = pgTzSet(field)
					if namedTz == nil {
						extra.timezone = field
						return dtype, tm, fsec, tzp, dtErrBadTimezone
					}
					tmask = dtkM(ftTZ)
				}
			} else if dterr := decodeDate(field, fmask, &tmask, &is2digits, &tm, sess); dterr != 0 {
				return dtype, tm, fsec, tzp, dterr
			}
		case dtkTime:
			if ptype != 0 {
				if ptype != dtkTime {
					return dtype, tm, fsec, tzp, dtErrBadFormat
				}
				ptype = 0
			}
			if dterr := decodeTime(field, intervalFullRange, &tmask, &tm, &fsec); dterr != 0 {
				return dtype, tm, fsec, tzp, dterr
			}
			if timeOverflows(tm.hour, tm.min, tm.sec, fsec) {
				return dtype, tm, fsec, tzp, dtErrFieldOverflow
			}
		case dtkTZ:
			tz, dterr := decodeTimezone(field)
			if dterr != 0 {
				return dtype, tm, fsec, tzp, dterr
			}
			tzp = tz
			tmask = dtkM(ftTZ)
		case dtkNumber:
			if ptype != 0 {
				v, cp, erange := strtoNum(field, 32)
				if erange {
					return dtype, tm, fsec, tzp, dtErrFieldOverflow
				}
				if cp != "" && cp[0] != '.' {
					return dtype, tm, fsec, tzp, dtErrBadFormat
				}
				value := int(v)
				switch ptype {
				case dtkJulian:
					if value < 0 {
						return dtype, tm, fsec, tzp, dtErrFieldOverflow
					}
					tmask = dtkDateM
					tm.year, tm.mon, tm.mday = j2date(value)
					isjulian = true
					if cp != "" {
						f, dterr := parseFraction(cp)
						if dterr != 0 {
							return dtype, tm, fsec, tzp, dterr
						}
						tm.hour, tm.min, tm.sec, fsec = dt2time(int64(f * float64(usecsPerDay)))
						tmask |= dtkTimeM
					}
				case dtkTime:
					if dterr := decodeNumberField(len(field), field, fmask|dtkDateM, &tmask, &tm, &fsec, &is2digits); dterr < 0 {
						return dtype, tm, fsec, tzp, dterr
					}
					if tmask != dtkTimeM {
						return dtype, tm, fsec, tzp, dtErrBadFormat
					}
				default:
					return dtype, tm, fsec, tzp, dtErrBadFormat
				}
				ptype = 0
				dtype = dtkDate
			} else {
				flen := len(field)
				dot := strings.IndexByte(field, '.')
				switch {
				case dot >= 0 && fmask&dtkDateM == 0:
					if dterr := decodeDate(field, fmask, &tmask, &is2digits, &tm, sess); dterr != 0 {
						return dtype, tm, fsec, tzp, dterr
					}
				case dot >= 0 && dot > 2:
					if dterr := decodeNumberField(flen, field, fmask, &tmask, &tm, &fsec, &is2digits); dterr < 0 {
						return dtype, tm, fsec, tzp, dterr
					}
				case flen >= 6 && (fmask&dtkDateM == 0 || fmask&dtkTimeM == 0):
					if dterr := decodeNumberField(flen, field, fmask, &tmask, &tm, &fsec, &is2digits); dterr < 0 {
						return dtype, tm, fsec, tzp, dterr
					}
				default:
					if dterr := decodeNumber(flen, field, haveTextMonth, fmask, &tmask, &tm, &fsec, &is2digits, sess); dterr != 0 {
						return dtype, tm, fsec, tzp, dterr
					}
				}
			}
		case dtkString, dtkSpecial:
			typ, val, valtz, dterr := decodeTimezoneAbbrev(field, extra)
			if dterr != 0 {
				return dtype, tm, fsec, tzp, dterr
			}
			if typ == ftUnknown {
				typ, val = decodeSpecial(field)
			}
			if typ == ftIgnore {
				continue
			}
			tmask = dtkM(typ)
			switch typ {
			case ftReserv:
				switch val {
				case dtkNow:
					tmask = dtkDateM | dtkTimeM | dtkM(ftTZ)
					dtype = dtkDate
					tm = currentTM(sess.tz)
					tzp = determineTimeZoneOffset(&tm, sess.tz)
				case dtkYesterday, dtkToday, dtkTomorrow:
					tmask = dtkDateM
					dtype = dtkDate
					cur := currentTM(sess.tz)
					d := map[int]int{dtkYesterday: -1, dtkToday: 0, dtkTomorrow: 1}[val]
					tm.year, tm.mon, tm.mday = j2date(date2j(cur.year, cur.mon, cur.mday) + d)
				case dtkZulu:
					tmask = dtkTimeM | dtkM(ftTZ)
					dtype = dtkDate
					tm.hour, tm.min, tm.sec = 0, 0, 0
					tzp = 0
				case dtkEpoch, dtkLate, dtkEarly:
					tmask = dtkDateM | dtkTimeM | dtkM(ftTZ)
					dtype = val
				default:
					return dtype, tm, fsec, tzp, dtErrBadFormat
				}
			case ftMonth:
				if fmask&dtkM(ftMonth) != 0 && !haveTextMonth && fmask&dtkM(ftDay) == 0 && tm.mon >= 1 && tm.mon <= 31 {
					tm.mday = tm.mon
					tmask = dtkM(ftDay)
				}
				haveTextMonth = true
				tm.mon = val
			case ftDTZMod:
				tmask |= dtkM(ftDTZ)
				tm.isdst = 1
				tzp -= val
			case ftDTZ:
				tmask |= dtkM(ftTZ)
				tm.isdst = 1
				tzp = -val
			case ftTZ:
				tm.isdst = 0
				tzp = -val
			case ftDynTZ:
				tmask |= dtkM(ftTZ)
				abbrevTz = valtz
			case ftAMPM:
				mer = val
			case ftADBC:
				bc = val == adBC
			case ftDOW:
				tm.wday = val
			case ftUnits:
				tmask = 0
				if ptype != 0 {
					return dtype, tm, fsec, tzp, dtErrBadFormat
				}
				ptype = val
			case ftISOTime:
				tmask = 0
				if fmask&dtkDateM != dtkDateM {
					return dtype, tm, fsec, tzp, dtErrBadFormat
				}
				if ptype != 0 {
					return dtype, tm, fsec, tzp, dtErrBadFormat
				}
				ptype = val
			case ftUnknown:
				namedTz = pgTzSet(field)
				if namedTz == nil {
					return dtype, tm, fsec, tzp, dtErrBadFormat
				}
				tmask = dtkM(ftTZ)
			default:
				return dtype, tm, fsec, tzp, dtErrBadFormat
			}
		default:
			return dtype, tm, fsec, tzp, dtErrBadFormat
		}
		if tmask&fmask != 0 {
			return dtype, tm, fsec, tzp, dtErrBadFormat
		}
		fmask |= tmask
	}
	if ptype != 0 {
		return dtype, tm, fsec, tzp, dtErrBadFormat
	}
	if dtype == dtkDate {
		if dterr := validateDate(fmask, isjulian, is2digits, bc, &tm); dterr != 0 {
			return dtype, tm, fsec, tzp, dterr
		}
		if mer != merHR24 && tm.hour > hoursPerDay/2 {
			return dtype, tm, fsec, tzp, dtErrFieldOverflow
		}
		if mer == merAM && tm.hour == hoursPerDay/2 {
			tm.hour = 0
		} else if mer == merPM && tm.hour != hoursPerDay/2 {
			tm.hour += hoursPerDay / 2
		}
		if fmask&dtkDateM != dtkDateM {
			if fmask&dtkTimeM == dtkTimeM {
				return dtype, tm, fsec, tzp, 1
			}
			return dtype, tm, fsec, tzp, dtErrBadFormat
		}
		if namedTz != nil {
			if fmask&dtkM(ftDTZMod) != 0 {
				return dtype, tm, fsec, tzp, dtErrBadFormat
			}
			tzp = determineTimeZoneOffset(&tm, namedTz)
		}
		if abbrevTz != nil {
			if fmask&dtkM(ftDTZMod) != 0 {
				return dtype, tm, fsec, tzp, dtErrBadFormat
			}
			tzp = determineTimeZoneOffset(&tm, abbrevTz)
		}
		if fmask&dtkM(ftTZ) == 0 {
			if fmask&dtkM(ftDTZMod) != 0 {
				return dtype, tm, fsec, tzp, dtErrBadFormat
			}
			tzp = determineTimeZoneOffset(&tm, sess.tz)
		}
	}
	return dtype, tm, fsec, tzp, 0
}

// --- DecodeTimeOnly ------------------------------------------------------------------

func decodeTimeOnly(fields []string, ftypes []int, sess *dtSession, extra *dtExtra) (tm pgTM, fsec int64, tzp int, ret int) {
	fmask := 0
	ptype := 0
	mer := merHR24
	isjulian, is2digits, bc := false, false, false
	var namedTz, abbrevTz *time.Location
	tm.isdst = -1
	nf := len(fields)

	for i := range fields {
		field := fields[i]
		tmask := 0
		switch ftypes[i] {
		case dtkDate:
			if i == 0 && nf >= 2 && (ftypes[nf-1] == dtkDate || ftypes[1] == dtkTime) {
				if dterr := decodeDate(field, fmask, &tmask, &is2digits, &tm, sess); dterr != 0 {
					return tm, fsec, tzp, dterr
				}
			} else if cIsDigit(field[0]) {
				if fmask&dtkTimeM == dtkTimeM {
					return tm, fsec, tzp, dtErrBadFormat
				}
				dash := strings.IndexByte(field, '-')
				if dash < 0 {
					return tm, fsec, tzp, dtErrBadFormat
				}
				var dterr int
				if tzp, dterr = decodeTimezone(field[dash:]); dterr != 0 {
					return tm, fsec, tzp, dterr
				}
				head := field[:dash]
				dterr = decodeNumberField(len(head), head, fmask|dtkDateM, &tmask, &tm, &fsec, &is2digits)
				if dterr < 0 {
					return tm, fsec, tzp, dterr
				}
				ftypes[i] = dterr
				tmask |= dtkM(ftTZ)
			} else {
				namedTz = pgTzSet(field)
				if namedTz == nil {
					extra.timezone = field
					return tm, fsec, tzp, dtErrBadTimezone
				}
				ftypes[i] = dtkTZ
				tmask = dtkM(ftTZ)
			}
		case dtkTime:
			if ptype != 0 {
				if ptype != dtkTime {
					return tm, fsec, tzp, dtErrBadFormat
				}
				ptype = 0
			}
			if dterr := decodeTime(field, intervalFullRange, &tmask, &tm, &fsec); dterr != 0 {
				return tm, fsec, tzp, dterr
			}
		case dtkTZ:
			tz, dterr := decodeTimezone(field)
			if dterr != 0 {
				return tm, fsec, tzp, dterr
			}
			tzp = tz
			tmask = dtkM(ftTZ)
		case dtkNumber:
			if ptype != 0 {
				v, cp, erange := strtoNum(field, 32)
				if erange {
					return tm, fsec, tzp, dtErrFieldOverflow
				}
				if cp != "" && cp[0] != '.' {
					return tm, fsec, tzp, dtErrBadFormat
				}
				value := int(v)
				switch ptype {
				case dtkJulian:
					if value < 0 {
						return tm, fsec, tzp, dtErrFieldOverflow
					}
					tmask = dtkDateM
					tm.year, tm.mon, tm.mday = j2date(value)
					isjulian = true
					if cp != "" {
						f, dterr := parseFraction(cp)
						if dterr != 0 {
							return tm, fsec, tzp, dterr
						}
						tm.hour, tm.min, tm.sec, fsec = dt2time(int64(f * float64(usecsPerDay)))
						tmask |= dtkTimeM
					}
				case dtkTime:
					dterr := decodeNumberField(len(field), field, fmask|dtkDateM, &tmask, &tm, &fsec, &is2digits)
					if dterr < 0 {
						return tm, fsec, tzp, dterr
					}
					ftypes[i] = dterr
					if tmask != dtkTimeM {
						return tm, fsec, tzp, dtErrBadFormat
					}
				default:
					return tm, fsec, tzp, dtErrBadFormat
				}
				ptype = 0
			} else {
				flen := len(field)
				dot := strings.IndexByte(field, '.')
				switch {
				case dot >= 0:
					if i == 0 && nf >= 2 && ftypes[nf-1] == dtkDate {
						if dterr := decodeDate(field, fmask, &tmask, &is2digits, &tm, sess); dterr != 0 {
							return tm, fsec, tzp, dterr
						}
					} else if dot > 2 {
						dterr := decodeNumberField(flen, field, fmask|dtkDateM, &tmask, &tm, &fsec, &is2digits)
						if dterr < 0 {
							return tm, fsec, tzp, dterr
						}
						ftypes[i] = dterr
					} else {
						return tm, fsec, tzp, dtErrBadFormat
					}
				case flen > 4:
					dterr := decodeNumberField(flen, field, fmask|dtkDateM, &tmask, &tm, &fsec, &is2digits)
					if dterr < 0 {
						return tm, fsec, tzp, dterr
					}
					ftypes[i] = dterr
				default:
					if dterr := decodeNumber(flen, field, false, fmask|dtkDateM, &tmask, &tm, &fsec, &is2digits, sess); dterr != 0 {
						return tm, fsec, tzp, dterr
					}
				}
			}
		case dtkString, dtkSpecial:
			typ, val, valtz, dterr := decodeTimezoneAbbrev(field, extra)
			if dterr != 0 {
				return tm, fsec, tzp, dterr
			}
			if typ == ftUnknown {
				typ, val = decodeSpecial(field)
			}
			if typ == ftIgnore {
				continue
			}
			tmask = dtkM(typ)
			switch typ {
			case ftReserv:
				switch val {
				case dtkNow:
					tmask = dtkTimeM
					cur := currentTM(sess.tz)
					tm.hour, tm.min, tm.sec = cur.hour, cur.min, cur.sec
				case dtkZulu:
					tmask = dtkTimeM | dtkM(ftTZ)
					tm.hour, tm.min, tm.sec = 0, 0, 0
					tm.isdst = 0
				default:
					return tm, fsec, tzp, dtErrBadFormat
				}
			case ftDTZMod:
				tmask |= dtkM(ftDTZ)
				tm.isdst = 1
				tzp -= val
			case ftDTZ:
				tmask |= dtkM(ftTZ)
				tm.isdst = 1
				tzp = -val
				ftypes[i] = dtkTZ
			case ftTZ:
				tm.isdst = 0
				tzp = -val
				ftypes[i] = dtkTZ
			case ftDynTZ:
				tmask |= dtkM(ftTZ)
				abbrevTz = valtz
				ftypes[i] = dtkTZ
			case ftAMPM:
				mer = val
			case ftADBC:
				bc = val == adBC
			case ftUnits, ftISOTime:
				tmask = 0
				if ptype != 0 {
					return tm, fsec, tzp, dtErrBadFormat
				}
				ptype = val
			case ftUnknown:
				namedTz = pgTzSet(field)
				if namedTz == nil {
					return tm, fsec, tzp, dtErrBadFormat
				}
				tmask = dtkM(ftTZ)
			default:
				return tm, fsec, tzp, dtErrBadFormat
			}
		default:
			return tm, fsec, tzp, dtErrBadFormat
		}
		if tmask&fmask != 0 {
			return tm, fsec, tzp, dtErrBadFormat
		}
		fmask |= tmask
	}
	if ptype != 0 {
		return tm, fsec, tzp, dtErrBadFormat
	}
	if dterr := validateDate(fmask, isjulian, is2digits, bc, &tm); dterr != 0 {
		return tm, fsec, tzp, dterr
	}
	if mer != merHR24 && tm.hour > hoursPerDay/2 {
		return tm, fsec, tzp, dtErrFieldOverflow
	}
	if mer == merAM && tm.hour == hoursPerDay/2 {
		tm.hour = 0
	} else if mer == merPM && tm.hour != hoursPerDay/2 {
		tm.hour += hoursPerDay / 2
	}
	if timeOverflows(tm.hour, tm.min, tm.sec, fsec) {
		return tm, fsec, tzp, dtErrFieldOverflow
	}
	if fmask&dtkTimeM != dtkTimeM {
		return tm, fsec, tzp, dtErrBadFormat
	}
	dateFor := func() (pgTM, int) {
		if fmask&dtkDateM == 0 {
			cur := currentTM(sess.tz)
			cur.hour, cur.min, cur.sec = tm.hour, tm.min, tm.sec
			return cur, 0
		}
		if fmask&dtkDateM != dtkDateM {
			return tm, dtErrBadFormat
		}
		return tm, 0
	}
	if namedTz != nil {
		if fmask&dtkM(ftDTZMod) != 0 {
			return tm, fsec, tzp, dtErrBadFormat
		}
		// a fixed-offset zone needs no date; PG demands a full date for others
		if fixed, off := isFixedZone(namedTz); fixed {
			tzp = -off
		} else {
			if fmask&dtkDateM != dtkDateM {
				return tm, fsec, tzp, dtErrBadFormat
			}
			tzp = determineTimeZoneOffset(&tm, namedTz)
		}
	}
	if abbrevTz != nil {
		if fmask&dtkM(ftDTZMod) != 0 {
			return tm, fsec, tzp, dtErrBadFormat
		}
		t, dterr := dateFor()
		if dterr != 0 {
			return tm, fsec, tzp, dterr
		}
		tzp = determineTimeZoneOffset(&t, abbrevTz)
	}
	if fmask&dtkM(ftTZ) == 0 {
		if fmask&dtkM(ftDTZMod) != 0 {
			return tm, fsec, tzp, dtErrBadFormat
		}
		t, dterr := dateFor()
		if dterr != 0 {
			return tm, fsec, tzp, dterr
		}
		tzp = determineTimeZoneOffset(&t, sess.tz)
	}
	return tm, fsec, tzp, 0
}

// isFixedZone approximates pg_get_timezone_offset: true when the zone never changes
// its offset (checked over a wide range of instants).
func isFixedZone(loc *time.Location) (bool, int) {
	_, off := time.Date(2000, 1, 1, 0, 0, 0, 0, loc).Zone()
	for _, y := range []int{1900, 1950, 1980, 2000, 2010, 2020, 2024, 2030} {
		for _, m := range []time.Month{1, 4, 7, 10} {
			if _, o := time.Date(y, m, 1, 0, 0, 0, 0, loc).Zone(); o != off {
				return false, 0
			}
		}
	}
	return true, off
}

// --- DecodeInterval ------------------------------------------------------------------

func decodeInterval(fields []string, ftypes []int, rng int, sess *dtSession) (dtype int, in itmIn, ret int) {
	forceNegative, isBefore, parsingUnitVal := false, false, false
	fmask := 0
	typ := ftIgnore
	dtype = dtkDelta
	nf := len(fields)

	if sess.sqlStandard && nf > 0 && fields[0][0] == '-' {
		forceNegative = true
		for i := 1; i < nf; i++ {
			if fields[i][0] == '-' || fields[i][0] == '+' {
				forceNegative = false
				break
			}
		}
	}
	for i := nf - 1; i >= 0; i-- {
		field := fields[i]
		tmask := 0
		ft := ftypes[i]
		if ft == dtkTZ && strings.IndexByte(field[1:], ':') >= 0 {
			var tm2 int
			if decodeTimeForInterval(field[1:], rng, &tm2, &in) == 0 {
				tmask = tm2
				if field[0] == '-' {
					if in.usec == math.MinInt64 {
						return dtype, in, dtErrFieldOverflow
					}
					in.usec = -in.usec
				}
				if forceNegative && in.usec > 0 {
					in.usec = -in.usec
				}
				typ = dtkDay
				parsingUnitVal = false
				goto next
			}
			ft = dtkNumber // fall through
		}
		switch ft {
		case dtkTime:
			if dterr := decodeTimeForInterval(field, rng, &tmask, &in); dterr != 0 {
				return dtype, in, dterr
			}
			if forceNegative && in.usec > 0 {
				in.usec = -in.usec
			}
			typ = dtkDay
			parsingUnitVal = false
		case dtkTZ, dtkDate, dtkNumber:
			if typ == ftIgnore {
				switch rng {
				case imYear:
					typ = dtkYear
				case imMonth, imYear | imMonth:
					typ = dtkMonth
				case imDay:
					typ = dtkDay
				case imHour, imDay | imHour:
					typ = dtkHour
				case imMinute, imHour | imMinute, imDay | imHour | imMinute:
					typ = dtkMinute
				default:
					typ = dtkSecond
				}
			}
			val, cp, erange := strtoNum(field, 64)
			if erange {
				return dtype, in, dtErrFieldOverflow
			}
			var fval float64
			switch {
			case cp != "" && cp[0] == '-':
				v2, cp2, erange := strtoNum(cp[1:], 32)
				if erange || v2 < 0 || v2 >= monthsPerYear {
					return dtype, in, dtErrFieldOverflow
				}
				if cp2 != "" {
					return dtype, in, dtErrBadFormat
				}
				typ = dtkMonth
				if field[0] == '-' {
					v2 = -v2
				}
				var over bool
				if val, over = mul64(val, monthsPerYear); over {
					return dtype, in, dtErrFieldOverflow
				}
				if val, over = add64(val, v2); over {
					return dtype, in, dtErrFieldOverflow
				}
			case cp != "" && cp[0] == '.':
				var dterr int
				if fval, dterr = parseFraction(cp); dterr != 0 {
					return dtype, in, dterr
				}
				if field[0] == '-' {
					fval = -fval
				}
			case cp == "":
			default:
				return dtype, in, dtErrBadFormat
			}
			if forceNegative {
				if val > 0 {
					val = -val
				}
				if fval > 0 {
					fval = -fval
				}
			}
			ok := true
			switch typ {
			case dtkMicrosec:
				ok = adjustMicroseconds(val, fval, 1, &in)
				tmask = dtkM(ftMicrosecond)
			case dtkMillisec:
				ok = adjustMicroseconds(val, fval, 1000, &in)
				tmask = dtkM(ftMillisecond)
			case dtkSecond:
				ok = adjustMicroseconds(val, fval, usecsPerSec, &in)
				if fval == 0 {
					tmask = dtkM(ftSecond)
				} else {
					tmask = dtkAllSecsM
				}
			case dtkMinute:
				ok = adjustMicroseconds(val, fval, usecsPerMin, &in)
				tmask = dtkM(ftMinute)
			case dtkHour:
				ok = adjustMicroseconds(val, fval, usecsPerHour, &in)
				tmask = dtkM(ftHour)
				typ = dtkDay
			case dtkDay:
				ok = adjustDays(val, 1, &in) && adjustFractMicroseconds(fval, usecsPerDay, &in)
				tmask = dtkM(ftDay)
			case dtkWeek:
				ok = adjustDays(val, 7, &in) && adjustFractDays(fval, 7, &in)
				tmask = dtkM(ftWeek)
			case dtkMonth:
				ok = adjustMonths(val, &in) && adjustFractDays(fval, daysPerMonth, &in)
				tmask = dtkM(ftMonth)
			case dtkYear:
				ok = adjustYears(val, 1, &in) && adjustFractYears(fval, 1, &in)
				tmask = dtkM(ftYear)
			case dtkDecade:
				ok = adjustYears(val, 10, &in) && adjustFractYears(fval, 10, &in)
				tmask = dtkM(ftDecade)
			case dtkCentury:
				ok = adjustYears(val, 100, &in) && adjustFractYears(fval, 100, &in)
				tmask = dtkM(ftCentury)
			case dtkMillennium:
				ok = adjustYears(val, 1000, &in) && adjustFractYears(fval, 1000, &in)
				tmask = dtkM(ftMillennium)
			default:
				return dtype, in, dtErrBadFormat
			}
			if !ok {
				return dtype, in, dtErrFieldOverflow
			}
			parsingUnitVal = false
		case dtkString, dtkSpecial:
			if parsingUnitVal {
				return dtype, in, dtErrBadFormat
			}
			t, uval := decodeUnits(field)
			if t == ftUnknown {
				t, uval = decodeSpecial(field)
			}
			if t == ftIgnore {
				continue
			}
			switch t {
			case ftUnits:
				typ = uval
				parsingUnitVal = true
			case ftAgo:
				if i != nf-1 {
					return dtype, in, dtErrBadFormat
				}
				isBefore = true
				typ = uval
			case ftReserv:
				tmask = dtkDateM | dtkTimeM
				if uval != dtkLate && uval != dtkEarly {
					return dtype, in, dtErrBadFormat
				}
				if i != nf-1 {
					return dtype, in, dtErrBadFormat
				}
				dtype = uval
			default:
				return dtype, in, dtErrBadFormat
			}
		default:
			return dtype, in, dtErrBadFormat
		}
	next:
		if tmask&fmask != 0 {
			return dtype, in, dtErrBadFormat
		}
		fmask |= tmask
	}
	if fmask == 0 {
		return dtype, in, dtErrBadFormat
	}
	if parsingUnitVal {
		return dtype, in, dtErrBadFormat
	}
	if isBefore {
		if in.usec == math.MinInt64 || in.mday == math.MinInt32 || in.mon == math.MinInt32 || in.year == math.MinInt32 {
			return dtype, in, dtErrFieldOverflow
		}
		in.usec, in.mday, in.mon, in.year = -in.usec, -in.mday, -in.mon, -in.year
	}
	return dtype, in, 0
}

// --- DecodeISO8601Interval -----------------------------------------------------------

func parseISO8601Number(str string) (rest string, ipart int64, fpart float64, dterr int) {
	if str == "" || !(cIsDigit(str[0]) || str[0] == '-' || str[0] == '.') {
		return str, 0, 0, dtErrBadFormat
	}
	val, rest, ok := strtod(str)
	if !ok || len(rest) == len(str) {
		return str, 0, 0, dtErrBadFormat
	}
	if math.IsNaN(val) || val < -1.0e15 || val > 1.0e15 {
		return str, 0, 0, dtErrFieldOverflow
	}
	if val >= 0 {
		ipart = int64(math.Floor(val))
	} else {
		ipart = -int64(math.Floor(-val))
	}
	return rest, ipart, val - float64(ipart), 0
}

func iso8601IntegerWidth(fieldstart string) int {
	fieldstart = strings.TrimPrefix(fieldstart, "-")
	n := 0
	for n < len(fieldstart) && cIsDigit(fieldstart[n]) {
		n++
	}
	return n
}

func decodeISO8601Interval(str string) (dtype int, in itmIn, ret int) {
	dtype = dtkDelta
	datepart, havefield := true, false
	if len(str) < 2 || str[0] != 'P' {
		return dtype, in, dtErrBadFormat
	}
	str = str[1:]
	fail := func(ok bool) int {
		if ok {
			return 0
		}
		return dtErrFieldOverflow
	}
	for str != "" {
		if str[0] == 'T' {
			datepart, havefield = false, false
			str = str[1:]
			continue
		}
		fieldstart := str
		var val int64
		var fval float64
		var dterr int
		if str, val, fval, dterr = parseISO8601Number(str); dterr != 0 {
			return dtype, in, dterr
		}
		var unit byte
		if str != "" {
			unit = str[0]
			str = str[1:]
		}
		if datepart {
			switch unit {
			case 'Y':
				if d := fail(adjustYears(val, 1, &in) && adjustFractYears(fval, 1, &in)); d != 0 {
					return dtype, in, d
				}
			case 'M':
				if d := fail(adjustMonths(val, &in) && adjustFractDays(fval, daysPerMonth, &in)); d != 0 {
					return dtype, in, d
				}
			case 'W':
				if d := fail(adjustDays(val, 7, &in) && adjustFractDays(fval, 7, &in)); d != 0 {
					return dtype, in, d
				}
			case 'D':
				if d := fail(adjustDays(val, 1, &in) && adjustFractMicroseconds(fval, usecsPerDay, &in)); d != 0 {
					return dtype, in, d
				}
			case 'T', 0, '-':
				if (unit == 'T' || unit == 0) && iso8601IntegerWidth(fieldstart) == 8 && !havefield {
					if d := fail(adjustYears(val/10000, 1, &in) && adjustMonths((val/100)%100, &in) &&
						adjustDays(val%100, 1, &in) && adjustFractMicroseconds(fval, usecsPerDay, &in)); d != 0 {
						return dtype, in, d
					}
					if unit == 0 {
						return dtype, in, 0
					}
					datepart, havefield = false, false
					continue
				}
				if havefield {
					return dtype, in, dtErrBadFormat
				}
				if d := fail(adjustYears(val, 1, &in) && adjustFractYears(fval, 1, &in)); d != 0 {
					return dtype, in, d
				}
				if unit == 0 {
					return dtype, in, 0
				}
				if unit == 'T' {
					datepart, havefield = false, false
					continue
				}
				if str, val, fval, dterr = parseISO8601Number(str); dterr != 0 {
					return dtype, in, dterr
				}
				if d := fail(adjustMonths(val, &in) && adjustFractDays(fval, daysPerMonth, &in)); d != 0 {
					return dtype, in, d
				}
				if str == "" {
					return dtype, in, 0
				}
				if str[0] == 'T' {
					datepart, havefield = false, false
					str = str[1:]
					continue
				}
				if str[0] != '-' {
					return dtype, in, dtErrBadFormat
				}
				str = str[1:]
				if str, val, fval, dterr = parseISO8601Number(str); dterr != 0 {
					return dtype, in, dterr
				}
				if d := fail(adjustDays(val, 1, &in) && adjustFractMicroseconds(fval, usecsPerDay, &in)); d != 0 {
					return dtype, in, d
				}
				if str == "" {
					return dtype, in, 0
				}
				if str[0] == 'T' {
					datepart, havefield = false, false
					str = str[1:]
					continue
				}
				return dtype, in, dtErrBadFormat
			default:
				return dtype, in, dtErrBadFormat
			}
		} else {
			switch unit {
			case 'H':
				if d := fail(adjustMicroseconds(val, fval, usecsPerHour, &in)); d != 0 {
					return dtype, in, d
				}
			case 'M':
				if d := fail(adjustMicroseconds(val, fval, usecsPerMin, &in)); d != 0 {
					return dtype, in, d
				}
			case 'S':
				if d := fail(adjustMicroseconds(val, fval, usecsPerSec, &in)); d != 0 {
					return dtype, in, d
				}
			case 0, ':':
				if unit == 0 && iso8601IntegerWidth(fieldstart) == 6 && !havefield {
					if d := fail(adjustMicroseconds(val/10000, 0, usecsPerHour, &in) &&
						adjustMicroseconds((val/100)%100, 0, usecsPerMin, &in) &&
						adjustMicroseconds(val%100, 0, usecsPerSec, &in) &&
						adjustFractMicroseconds(fval, 1, &in)); d != 0 {
						return dtype, in, d
					}
					return dtype, in, 0
				}
				if havefield {
					return dtype, in, dtErrBadFormat
				}
				if d := fail(adjustMicroseconds(val, fval, usecsPerHour, &in)); d != 0 {
					return dtype, in, d
				}
				if unit == 0 {
					return dtype, in, 0
				}
				if str, val, fval, dterr = parseISO8601Number(str); dterr != 0 {
					return dtype, in, dterr
				}
				if d := fail(adjustMicroseconds(val, fval, usecsPerMin, &in)); d != 0 {
					return dtype, in, d
				}
				if str == "" {
					return dtype, in, 0
				}
				if str[0] != ':' {
					return dtype, in, dtErrBadFormat
				}
				str = str[1:]
				if str, val, fval, dterr = parseISO8601Number(str); dterr != 0 {
					return dtype, in, dterr
				}
				if d := fail(adjustMicroseconds(val, fval, usecsPerSec, &in)); d != 0 {
					return dtype, in, d
				}
				if str == "" {
					return dtype, in, 0
				}
				return dtype, in, dtErrBadFormat
			default:
				return dtype, in, dtErrBadFormat
			}
		}
		havefield = true
	}
	return dtype, in, 0
}

// --- the input functions ---------------------------------------------------------------

// dtError is the SQLSTATE and message DateTimeParseError raises for dterr.
func dtError(dterr int, extra *dtExtra, str, datatype string, loc int32) *Error {
	switch dterr {
	case dtErrFieldOverflow, dtErrMDFieldOverflow:
		return errAt("22008", loc, "date/time field value out of range: %q", str)
	case dtErrIntervalOverflow:
		return errAt("22015", loc, "interval field value out of range: %q", str)
	case dtErrTZDispOverflow:
		return errAt("22009", loc, "time zone displacement out of range: %q", str)
	case dtErrBadTimezone:
		return errAt("22023", loc, "time zone %q not recognized", extra.timezone)
	case dtErrBadZoneAbbrev:
		return errAt("F0000", loc, "time zone %q not recognized", extra.timezone)
	}
	return errAt("22007", loc, "invalid input syntax for type %s: %q", datatype, str)
}

// tm2timestamp reports whether the broken-down time is a representable timestamp.
func tm2timestamp(tm *pgTM, fsec int64, tzp *int) bool {
	_, ok := timestampOf(tm, fsec, tzp)
	return ok
}

// timestampOf is tm2timestamp returning the value in microseconds since the PG epoch.
func timestampOf(tm *pgTM, fsec int64, tzp *int) (int64, bool) {
	if !isValidJulian(tm.year, tm.mon, tm.mday) {
		return 0, false
	}
	date := int64(date2j(tm.year, tm.mon, tm.mday) - postgresEpochJDate)
	t := (int64((tm.hour*minsPerHour+tm.min)*secsPerMinute+tm.sec) * usecsPerSec) + fsec
	prod, over := mul64(date, usecsPerDay)
	if over {
		return 0, false
	}
	result, over := add64(prod, t)
	if over || (result-t)/usecsPerDay != date {
		return 0, false
	}
	if (result < 0 && date > 0) || (result > 0 && date < -1) {
		return 0, false
	}
	if tzp != nil {
		result += int64(*tzp) * usecsPerSec
	}
	return result, minTimestamp <= result && result < endTimestamp
}

// validateTimestampLiteral is timestamp_in / timestamptz_in.
func validateTimestampLiteral(str string, withTZ bool, sess *dtSession, loc int32) *Error {
	name := "timestamp"
	if withTZ {
		name = "timestamp with time zone"
	}
	var extra dtExtra
	fields, ftypes, dterr := parseDateTime(str, maxDateLen+maxDateFields)
	var dtype int
	var tm pgTM
	var fsec int64
	var tz int
	if dterr == 0 {
		dtype, tm, fsec, tz, dterr = decodeDateTime(fields, ftypes, sess, &extra)
	}
	if dterr != 0 {
		return dtError(dterr, &extra, str, name, loc)
	}
	if dtype == dtkDate {
		var tzp *int
		if withTZ {
			tzp = &tz
		}
		if !tm2timestamp(&tm, fsec, tzp) {
			return errAt("22008", loc, "timestamp out of range: %q", str)
		}
	}
	return nil
}

// validateDateLiteral is date_in.
func validateDateLiteral(str string, sess *dtSession, loc int32) *Error {
	var extra dtExtra
	fields, ftypes, dterr := parseDateTime(str, maxDateLen+1)
	var dtype int
	var tm pgTM
	if dterr == 0 {
		dtype, tm, _, _, dterr = decodeDateTime(fields, ftypes, sess, &extra)
	}
	if dterr != 0 {
		return dtError(dterr, &extra, str, "date", loc)
	}
	switch dtype {
	case dtkDate:
	case dtkEpoch:
		tm = pgTM{year: 1970, mon: 1, mday: 1}
	case dtkLate, dtkEarly:
		return nil
	default:
		return dtError(dtErrBadFormat, &extra, str, "date", loc)
	}
	if !isValidJulian(tm.year, tm.mon, tm.mday) {
		return errAt("22008", loc, "date out of range: %q", str)
	}
	d := date2j(tm.year, tm.mon, tm.mday) - postgresEpochJDate
	if !(d >= -postgresEpochJDate && d < dateEndJulian-postgresEpochJDate) {
		return errAt("22008", loc, "date out of range: %q", str)
	}
	return nil
}

// validateTimeLiteral is time_in / timetz_in.
func validateTimeLiteral(str string, withTZ bool, sess *dtSession, loc int32) *Error {
	name := "time"
	if withTZ {
		name = "time with time zone"
	}
	var extra dtExtra
	fields, ftypes, dterr := parseDateTime(str, maxDateLen+1)
	if dterr == 0 {
		_, _, _, dterr = decodeTimeOnly(fields, ftypes, sess, &extra)
	}
	if dterr != 0 {
		return dtError(dterr, &extra, str, name, loc)
	}
	return nil
}

// validateIntervalLiteral is interval_in; typmod carries the field range.
func validateIntervalLiteral(str string, typmod int32, sess *dtSession, loc int32) *Error {
	rng := intervalFullRange
	if typmod >= 0 {
		rng = int(typmod>>16) & intervalFullRange
	}
	var extra dtExtra
	fields, ftypes, dterr := parseDateTime(str, 256)
	var dtype int
	var in itmIn
	if dterr == 0 {
		dtype, in, dterr = decodeInterval(fields, ftypes, rng, sess)
	}
	if dterr == dtErrBadFormat {
		dtype, in, dterr = decodeISO8601Interval(str)
	}
	if dterr != 0 {
		if dterr == dtErrFieldOverflow {
			dterr = dtErrIntervalOverflow
		}
		return dtError(dterr, &extra, str, "interval", loc)
	}
	if dtype == dtkDelta {
		total := int64(in.year)*monthsPerYear + int64(in.mon)
		if total > math.MaxInt32 || total < math.MinInt32 {
			return errAt("22008", loc, "interval out of range")
		}
		if typmod >= 0 {
			if prec := int(typmod) & intervalFullPrec; prec != intervalFullPrec && prec >= 0 && prec <= maxIntervalPrec {
				scale := []int64{1000000, 100000, 10000, 1000, 100, 10, 1}[prec]
				offs := []int64{500000, 50000, 5000, 500, 50, 5, 0}[prec]
				if in.usec >= 0 {
					if _, over := add64(in.usec, offs); over {
						return errAt("22008", loc, "interval out of range")
					}
				} else if _, over := add64(in.usec, -offs); over {
					return errAt("22008", loc, "interval out of range")
				}
				_ = scale
			}
		}
	}
	return nil
}

// dtSessionFor builds the GUC view the datetime input functions read.
func (a *analyzer) dtSession() *dtSession {
	if a.dts != nil {
		return a.dts
	}
	s := &dtSession{dateOrder: "mdy", tz: time.Local}
	if a.s != nil {
		order, style, zone := a.s.DateTimeSettings()
		if order != "" {
			s.dateOrder = order
		}
		s.sqlStandard = style == "sql_standard"
		if zone != "" {
			if loc := pgTzSet(zone); loc != nil {
				s.tz = loc
			} else if off, rest, ok := posixOffset(zone); ok && rest == "" {
				// SET TIME ZONE '+05:30' / '-8': an interval-style offset, ISO sign
				s.tz = time.FixedZone(zone, off)
			}
		}
	}
	a.dts = s
	return s
}

package analyze

// STR_TO_DATE over two constants, judged as item_timefunc.cc runs it (measured in
// TestFuncValServer): fix_from_format decides the result type from the format's
// specifiers (a time part makes a TIME or DATETIME, %f forces microseconds), and
// extract_date_time reads the value under the format. A failure -- a specifier that
// cannot read its field, a literal that does not match, a date check_date refuses under
// the session's zero-date flags -- is the warning 1411 ("Incorrect datetime value" from
// the outer call, "Incorrect time value" from the %r / %T sub-parse); a parsed value
// followed by a non-space tail is the 1292 make_truncated_value_warning raises, spelt
// with the result type's word. A strict write escalates either to the error
// (foldFuncVal's gate).

import (
	"fmt"
	"strings"
)

const maxDayNumber = 3652424

// my_locale_en_US's names, the only locale STR_TO_DATE's parse reads.
var (
	monthNames   = [...]string{"January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November", "December"}
	abMonthNames = [...]string{"Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"}
	dayNames     = [...]string{"Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday"}
	abDayNames   = [...]string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}
)

// strToDateFail judges STR_TO_DATE(val, format); nil when the parse succeeds or the
// checker cannot follow it.
func (a *analyzer) strToDateFail(val, format string, at int) *foldFail {
	kind := strings.ToLower(strToDateFormatType(format))
	var t mysqlTime
	flags := a.dateFlags("date") // TIME_FUZZY_DATE plus the mode's zero-date flags
	if kind == "time" {
		flags &^= timeNoZeroDate // val_datetime: a TIME result allows the zero date
	}
	word, truncated, ok := extractDateTime(format, val, &t, kind, flags, false)
	if !ok {
		return &foldFail{code: 1411, raw: fmt.Sprintf("Incorrect %s value: '%s' for function str_to_date", word, val), at: at}
	}
	if truncated {
		return &foldFail{code: 1292, raw: fmt.Sprintf("Truncated incorrect %s value: '%s'", kind, val), at: at}
	}
	if flags&timeNoZeroDate != 0 && kind != "time" && (t.year == 0 || t.month == 0 || t.day == 0) {
		// date_should_be_null: a parsed date with any zero part is refused under
		// NO_ZERO_DATE for a DATE / DATETIME result ('31.10.0000 15.30', measured)
		return &foldFail{code: 1411, raw: fmt.Sprintf("Incorrect datetime value: '%s' for function str_to_date", val), at: at}
	}
	return nil
}

// strToDateFormatType is Item_func_str_to_date::fix_from_format: the result's type from
// which specifier classes the format uses (expr.go's strToDateType reads it off the AST
// for typing; this one takes the format text the fold hands over).
func strToDateFormatType(format string) string {
	const timeParts = "HISThiklrs"
	const dateParts = "MVUXYWabcjmvuxyw"
	datePart, timePart, frac := false, false, false
	for i := 0; i < len(format); i++ {
		if format[i] == '%' && i+1 < len(format) {
			i++
			c := format[i]
			switch {
			case c == 'f':
				frac, timePart = true, true
			case strings.IndexByte(timeParts, c) >= 0:
				timePart = true
			case strings.IndexByte(dateParts, c) >= 0:
				datePart = true
			}
			if datePart && frac {
				return "DATETIME"
			}
		}
	}
	if frac {
		return "TIME"
	}
	if timePart {
		if datePart {
			return "DATETIME"
		}
		return "TIME"
	}
	return "DATE"
}

// extractDateTime is item_timefunc.cc's extract_date_time. word is the type word a
// failure spells ("datetime", or "time" from a %r / %T sub-parse), truncated reports a
// non-space tail after a successful parse, ok is false on a failure. sub returns after
// the format alone (the recursive %r / %T call).
func extractDateTime(format, val string, t *mysqlTime, kind string, flags timeFlags, sub bool) (word string, truncated, ok bool) {
	word = "datetime"
	if sub {
		word = "time"
	}
	weekday, yearday, daypart := 0, 0, uint(0)
	weekNumber := -1
	usaTime := false
	sundayFirst := false
	strictWeek := false
	strictWeekYear := -1
	strictWeekYearType := false
	v := 0 // position in val
	p := 0 // position in format
	for ; p < len(format) && v < len(val); p++ {
		for v < len(val) && val[v] == ' ' {
			v++ // skip pre-space before each field (MY_SEQ_SPACES)
		}
		if v >= len(val) {
			break
		}
		if format[p] == '%' && p+1 < len(format) {
			p++
			switch format[p] {
			case 'Y':
				n, width, good := readInt(val, &v, 4)
				if !good {
					return word, false, false
				}
				if width <= 2 {
					n = year2000(n)
				}
				t.year = uint(n)
			case 'y':
				n, _, good := readInt(val, &v, 2)
				if !good {
					return word, false, false
				}
				t.year = uint(year2000(n))
			case 'm', 'c':
				n, _, good := readInt(val, &v, 2)
				if !good {
					return word, false, false
				}
				t.month = uint(n)
			case 'M':
				m := checkWord(monthNames[:], val, &v)
				if m <= 0 {
					return word, false, false
				}
				t.month = uint(m)
			case 'b':
				m := checkWord(abMonthNames[:], val, &v)
				if m <= 0 {
					return word, false, false
				}
				t.month = uint(m)
			case 'd', 'e':
				n, _, good := readInt(val, &v, 2)
				if !good {
					return word, false, false
				}
				t.day = uint(n)
			case 'D':
				n, _, good := readInt(val, &v, 2)
				if !good {
					return word, false, false
				}
				t.day = uint(n)
				v += min(len(val)-v, 2) // skip st / nd / th
			case 'h', 'I', 'l':
				usaTime = true
				fallthrough
			case 'k', 'H':
				n, _, good := readInt(val, &v, 2)
				if !good {
					return word, false, false
				}
				t.hour = uint(n)
			case 'i':
				n, _, good := readInt(val, &v, 2)
				if !good {
					return word, false, false
				}
				t.minute = uint(n)
			case 's', 'S':
				n, _, good := readInt(val, &v, 2)
				if !good {
					return word, false, false
				}
				t.second = uint(n)
			case 'f':
				n, width, good := readInt(val, &v, 6)
				if !good {
					return word, false, false
				}
				for ; width < 6; width++ {
					n *= 10
				}
				t.secondPart = uint(n)
			case 'p':
				if len(val)-v < 2 || !usaTime {
					return word, false, false
				}
				switch strings.ToUpper(val[v : v+2]) {
				case "PM":
					daypart = 12
				case "AM":
				default:
					return word, false, false
				}
				v += 2
			case 'W':
				w := checkWord(dayNames[:], val, &v)
				if w <= 0 {
					return word, false, false
				}
				weekday = w
			case 'a':
				w := checkWord(abDayNames[:], val, &v)
				if w <= 0 {
					return word, false, false
				}
				weekday = w
			case 'w':
				n, _, good := readInt(val, &v, 1)
				if !good || n < 0 || n >= 7 {
					return word, false, false
				}
				weekday = n
				if weekday == 0 {
					weekday = 7
				}
			case 'j':
				n, _, good := readInt(val, &v, 3)
				if !good {
					return word, false, false
				}
				yearday = n
			case 'V', 'U', 'v', 'u':
				sundayFirst = format[p] == 'U' || format[p] == 'V'
				strictWeek = format[p] == 'V' || format[p] == 'v'
				n, _, good := readInt(val, &v, 2)
				if !good || n < 0 || (strictWeek && n == 0) || n > 53 {
					return word, false, false
				}
				weekNumber = n
			case 'X', 'x':
				strictWeekYearType = format[p] == 'X'
				n, _, good := readInt(val, &v, 4)
				if !good {
					return word, false, false
				}
				strictWeekYear = n
			case 'r':
				w, _, good := subExtract("%I:%i:%S %p", val, &v, t, kind, flags)
				if !good {
					return w, false, false
				}
			case 'T':
				w, _, good := subExtract("%H:%i:%S", val, &v, t, kind, flags)
				if !good {
					return w, false, false
				}
			case '.':
				for v < len(val) && isPunct(val[v]) {
					v++
				}
			case '@':
				for v < len(val) && isAlpha(val[v]) {
					v++
				}
			case '#':
				for v < len(val) && val[v] >= '0' && val[v] <= '9' {
					v++
				}
			default:
				return word, false, false
			}
		} else if format[p] != ' ' {
			if val[v] != format[p] {
				return word, false, false
			}
			v++
		}
	}
	if usaTime {
		if t.hour > 12 || t.hour < 1 {
			return word, false, false
		}
		t.hour = t.hour%12 + daypart
	}
	if sub {
		// the recursive %r / %T parse stops after its own format; hand the position back
		t.fields = v
		return word, false, true
	}
	if yearday > 0 {
		days := calcDaynr(t.year, 1, 1) + yearday - 1
		if days <= 0 || days > maxDayNumber {
			return word, false, false
		}
		t.year, t.month, t.day = dateFromDaynr(days)
	}
	if weekNumber >= 0 && weekday != 0 {
		if (strictWeek && (strictWeekYear < 0 || strictWeekYearType != sundayFirst)) ||
			(!strictWeek && strictWeekYear >= 0) {
			return word, false, false
		}
		year := t.year
		if strictWeek {
			year = uint(strictWeekYear)
		}
		days := calcDaynr(year, 1, 1)
		weekdayB := calcWeekday(days, sundayFirst)
		if sundayFirst {
			pad := 0
			if weekdayB != 0 {
				pad = 7
			}
			days += pad - weekdayB + (weekNumber-1)*7 + weekday%7
		} else {
			pad := 0
			if weekdayB > 3 {
				pad = 7
			}
			days += pad - weekdayB + (weekNumber-1)*7 + (weekday - 1)
		}
		if days <= 0 || days > maxDayNumber {
			return word, false, false
		}
		t.year, t.month, t.day = dateFromDaynr(days)
	}
	if kind == "time" {
		flags &^= timeNoZeroDate
	}
	var warn int
	if t.month > 12 || t.hour > 23 || t.minute > 59 || t.second > 59 ||
		checkDate(*t, t.year != 0 || t.month != 0 || t.day != 0, flags, &warn) {
		return word, false, false
	}
	for ; v < len(val); v++ {
		if val[v] != ' ' {
			return word, true, true
		}
	}
	return word, false, true
}

// subExtract runs the %r / %T sub-parse in place, advancing the caller's position.
func subExtract(subFormat, val string, v *int, t *mysqlTime, kind string, flags timeFlags) (string, bool, bool) {
	var tt mysqlTime
	tt = *t
	word, _, ok := extractDateTime(subFormat, val[*v:], &tt, kind, flags, true)
	if !ok {
		return word, false, false
	}
	consumed := tt.fields
	tt.fields = t.fields
	*t = tt
	*v += consumed
	return word, false, true
}

// readInt reads up to maxChars digits (my_strtoll10 bounded to val+min(maxChars, len)):
// at least one digit, the value and the digit count.
func readInt(val string, v *int, maxChars int) (n, width int, ok bool) {
	end := min(*v+maxChars, len(val))
	i := *v
	for i < end && val[i] >= '0' && val[i] <= '9' {
		n = n*10 + int(val[i]-'0')
		i++
	}
	if i == *v {
		return 0, 0, false
	}
	width = i - *v
	*v = i
	return n, width, true
}

// checkWord is my_time.cc's check_word: the 1-based index of the list word the value
// starts with, advancing past it; 0 when none matches.
func checkWord(words []string, val string, v *int) int {
	rest := val[*v:]
	for i, w := range words {
		if len(rest) >= len(w) && strings.EqualFold(rest[:len(w)], w) {
			*v += len(w)
			return i + 1
		}
	}
	return 0
}

// year2000 is year_2000_handling: a one- or two-digit year lands in 1970 .. 2069.
func year2000(y int) int {
	if y < 70 {
		return y + 2000
	}
	if y < 100 {
		return y + 1900
	}
	return y
}

func isAlpha(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

// calcDaynr is my_time.cc's calc_daynr: days since year 0.
func calcDaynr(year, month, day uint) int {
	if year == 0 && month == 0 {
		return 0
	}
	delsum := int(365*year) + 31*(int(month)-1) + int(day)
	y := int(year)
	if month <= 2 {
		y--
	} else {
		delsum -= (int(month)*4 + 23) / 10
	}
	return delsum + y/4 - (y/100+1)*3/4
}

// calcWeekday is my_time.cc's calc_weekday.
func calcWeekday(daynr int, sundayFirstDay bool) int {
	offset := 5
	if sundayFirstDay {
		offset = 6
	}
	return (daynr + offset) % 7
}

// dateFromDaynr is my_time.cc's get_date_from_daynr.
func dateFromDaynr(daynr int) (year, month, day uint) {
	if daynr <= 365 || daynr >= 3652500 {
		return 0, 0, 0
	}
	y := uint(daynr * 100 / 36525)
	temp := (((y - 1) / 100) + 1) * 3 / 4
	dayOfYear := uint(daynr) - y*365 - (y-1)/4 + temp
	daysThisYear := daysInYear(y)
	for dayOfYear > daysThisYear {
		dayOfYear -= daysThisYear
		y++
		daysThisYear = daysInYear(y)
	}
	leapDay := uint(0)
	if daysThisYear == 366 {
		if dayOfYear > 31+28 {
			dayOfYear--
			if dayOfYear == 31+28 {
				leapDay = 1
			}
		}
	}
	m := uint(1)
	for i := 0; dayOfYear > daysInMonth[i]; i++ {
		dayOfYear -= daysInMonth[i]
		m++
	}
	return y, m, dayOfYear + leapDay
}

func daysInYear(y uint) uint {
	if isLeap(y) {
		return 366
	}
	return 365
}

package analyze

// Token tables from PG's datetime.c (datetktbl / deltatktbl) and the default time zone
// abbreviation file (src/timezone/tznames/Default). Keys are lower case; lookups compare
// the first tokMaxLen bytes only, as datebsearch does with strncmp.

const tokMaxLen = 10

// field types (datetime.h)
const (
	ftReserv      = 0
	ftMonth       = 1
	ftYear        = 2
	ftDay         = 3
	ftJulian      = 4
	ftTZ          = 5
	ftDTZ         = 6
	ftDynTZ       = 7
	ftIgnore      = 8
	ftAMPM        = 9
	ftHour        = 10
	ftMinute      = 11
	ftSecond      = 12
	ftMillisecond = 13
	ftMicrosecond = 14
	ftDOY         = 15
	ftDOW         = 16
	ftUnits       = 17
	ftADBC        = 18
	ftAgo         = 19
	ftISOTime     = 23
	ftWeek        = 24
	ftDecade      = 25
	ftCentury     = 26
	ftMillennium  = 27
	ftDTZMod      = 28
	ftUnknown     = 31
)

// token values (DTK_*)
const (
	dtkNumber     = 0
	dtkString     = 1
	dtkDate       = 2
	dtkTime       = 3
	dtkTZ         = 4
	dtkSpecial    = 6
	dtkEarly      = 9
	dtkLate       = 10
	dtkEpoch      = 11
	dtkNow        = 12
	dtkYesterday  = 13
	dtkToday      = 14
	dtkTomorrow   = 15
	dtkZulu       = 16
	dtkDelta      = 17
	dtkSecond     = 18
	dtkMinute     = 19
	dtkHour       = 20
	dtkDay        = 21
	dtkWeek       = 22
	dtkMonth      = 23
	dtkQuarter    = 24
	dtkYear       = 25
	dtkDecade     = 26
	dtkCentury    = 27
	dtkMillennium = 28
	dtkMillisec   = 29
	dtkMicrosec   = 30
	dtkJulian     = 31
	dtkDow        = 32
	dtkDoy        = 33
	dtkTZHour     = 34
	dtkTZMinute   = 35
	dtkIsoYear    = 36
	dtkIsoDow     = 37
)

const (
	merAM   = 0
	merPM   = 1
	merHR24 = 2
	adAD    = 0
	adBC    = 1
)

type datetkn struct {
	typ   int
	value int
}

func dtkM(t int) int { return 1 << t }

var (
	dtkAllSecsM = dtkM(ftSecond) | dtkM(ftMillisecond) | dtkM(ftMicrosecond)
	dtkDateM    = dtkM(ftYear) | dtkM(ftMonth) | dtkM(ftDay)
	dtkTimeM    = dtkM(ftHour) | dtkM(ftMinute) | dtkAllSecsM
)

var datetktbl = map[string]datetkn{
	"+infinity": {ftReserv, dtkLate},
	"-infinity": {ftReserv, dtkEarly},
	"ad":        {ftADBC, adAD},
	"allballs":  {ftReserv, dtkZulu},
	"am":        {ftAMPM, merAM},
	"apr":       {ftMonth, 4},
	"april":     {ftMonth, 4},
	"at":        {ftIgnore, 0},
	"aug":       {ftMonth, 8},
	"august":    {ftMonth, 8},
	"bc":        {ftADBC, adBC},
	"d":         {ftUnits, dtkDay},
	"dec":       {ftMonth, 12},
	"december":  {ftMonth, 12},
	"dow":       {ftUnits, dtkDow},
	"doy":       {ftUnits, dtkDoy},
	"dst":       {ftDTZMod, 3600},
	"epoch":     {ftReserv, dtkEpoch},
	"feb":       {ftMonth, 2},
	"february":  {ftMonth, 2},
	"fri":       {ftDOW, 5},
	"friday":    {ftDOW, 5},
	"h":         {ftUnits, dtkHour},
	"infinity":  {ftReserv, dtkLate},
	"isodow":    {ftUnits, dtkIsoDow},
	"isoyear":   {ftUnits, dtkIsoYear},
	"j":         {ftUnits, dtkJulian},
	"jan":       {ftMonth, 1},
	"january":   {ftMonth, 1},
	"jd":        {ftUnits, dtkJulian},
	"jul":       {ftMonth, 7},
	"julian":    {ftUnits, dtkJulian},
	"july":      {ftMonth, 7},
	"jun":       {ftMonth, 6},
	"june":      {ftMonth, 6},
	"m":         {ftUnits, dtkMonth},
	"mar":       {ftMonth, 3},
	"march":     {ftMonth, 3},
	"may":       {ftMonth, 5},
	"mm":        {ftUnits, dtkMinute},
	"mon":       {ftDOW, 1},
	"monday":    {ftDOW, 1},
	"nov":       {ftMonth, 11},
	"november":  {ftMonth, 11},
	"now":       {ftReserv, dtkNow},
	"oct":       {ftMonth, 10},
	"october":   {ftMonth, 10},
	"on":        {ftIgnore, 0},
	"pm":        {ftAMPM, merPM},
	"s":         {ftUnits, dtkSecond},
	"sat":       {ftDOW, 6},
	"saturday":  {ftDOW, 6},
	"sep":       {ftMonth, 9},
	"sept":      {ftMonth, 9},
	"september": {ftMonth, 9},
	"sun":       {ftDOW, 0},
	"sunday":    {ftDOW, 0},
	"t":         {ftISOTime, dtkTime},
	"thu":       {ftDOW, 4},
	"thur":      {ftDOW, 4},
	"thurs":     {ftDOW, 4},
	"thursday":  {ftDOW, 4},
	"today":     {ftReserv, dtkToday},
	"tomorrow":  {ftReserv, dtkTomorrow},
	"tue":       {ftDOW, 2},
	"tues":      {ftDOW, 2},
	"tuesday":   {ftDOW, 2},
	"wed":       {ftDOW, 3},
	"wednesday": {ftDOW, 3},
	"weds":      {ftDOW, 3},
	"y":         {ftUnits, dtkYear},
	"yesterday": {ftReserv, dtkYesterday},
}

var deltatktbl = map[string]datetkn{
	"@":          {ftIgnore, 0},
	"ago":        {ftAgo, 0},
	"c":          {ftUnits, dtkCentury},
	"cent":       {ftUnits, dtkCentury},
	"centuries":  {ftUnits, dtkCentury},
	"century":    {ftUnits, dtkCentury},
	"d":          {ftUnits, dtkDay},
	"day":        {ftUnits, dtkDay},
	"days":       {ftUnits, dtkDay},
	"dec":        {ftUnits, dtkDecade},
	"decade":     {ftUnits, dtkDecade},
	"decades":    {ftUnits, dtkDecade},
	"decs":       {ftUnits, dtkDecade},
	"h":          {ftUnits, dtkHour},
	"hour":       {ftUnits, dtkHour},
	"hours":      {ftUnits, dtkHour},
	"hr":         {ftUnits, dtkHour},
	"hrs":        {ftUnits, dtkHour},
	"m":          {ftUnits, dtkMinute},
	"microsecon": {ftUnits, dtkMicrosec},
	"mil":        {ftUnits, dtkMillennium},
	"millennia":  {ftUnits, dtkMillennium},
	"millennium": {ftUnits, dtkMillennium},
	"millisecon": {ftUnits, dtkMillisec},
	"mils":       {ftUnits, dtkMillennium},
	"min":        {ftUnits, dtkMinute},
	"mins":       {ftUnits, dtkMinute},
	"minute":     {ftUnits, dtkMinute},
	"minutes":    {ftUnits, dtkMinute},
	"mon":        {ftUnits, dtkMonth},
	"mons":       {ftUnits, dtkMonth},
	"month":      {ftUnits, dtkMonth},
	"months":     {ftUnits, dtkMonth},
	"ms":         {ftUnits, dtkMillisec},
	"msec":       {ftUnits, dtkMillisec},
	"msecond":    {ftUnits, dtkMillisec},
	"mseconds":   {ftUnits, dtkMillisec},
	"msecs":      {ftUnits, dtkMillisec},
	"qtr":        {ftUnits, dtkQuarter},
	"quarter":    {ftUnits, dtkQuarter},
	"s":          {ftUnits, dtkSecond},
	"sec":        {ftUnits, dtkSecond},
	"second":     {ftUnits, dtkSecond},
	"seconds":    {ftUnits, dtkSecond},
	"secs":       {ftUnits, dtkSecond},
	"timezone":   {ftUnits, dtkTZ},
	"timezone_h": {ftUnits, dtkTZHour},
	"timezone_m": {ftUnits, dtkTZMinute},
	"us":         {ftUnits, dtkMicrosec},
	"usec":       {ftUnits, dtkMicrosec},
	"usecond":    {ftUnits, dtkMicrosec},
	"useconds":   {ftUnits, dtkMicrosec},
	"usecs":      {ftUnits, dtkMicrosec},
	"w":          {ftUnits, dtkWeek},
	"week":       {ftUnits, dtkWeek},
	"weeks":      {ftUnits, dtkWeek},
	"y":          {ftUnits, dtkYear},
	"year":       {ftUnits, dtkYear},
	"years":      {ftUnits, dtkYear},
	"yr":         {ftUnits, dtkYear},
	"yrs":        {ftUnits, dtkYear},
}

// lookupTok is datebsearch: keys longer than tokMaxLen match on their prefix.
func lookupTok(tbl map[string]datetkn, key string) (datetkn, bool) {
	if len(key) > tokMaxLen {
		key = key[:tokMaxLen]
	}
	t, ok := tbl[key]
	return t, ok
}

const (
	tzT    = ftTZ
	dtzT   = ftDTZ
	dyntzT = ftDynTZ
)

type tzAbbrev struct {
	typ    int
	offset int    // seconds east of Greenwich (TZ / DTZ)
	zone   string // DYNTZ: tzdb zone name
}

// tzAbbrevs is src/timezone/tznames/Default.
var tzAbbrevs = map[string]tzAbbrev{
	"eat":    {tzT, 10800, ""},
	"sast":   {tzT, 7200, ""},
	"wat":    {tzT, 3600, ""},
	"act":    {tzT, -18000, ""},
	"akdt":   {dtzT, -28800, ""},
	"akst":   {tzT, -32400, ""},
	"art":    {dyntzT, 0, "America/Argentina/Buenos_Aires"},
	"arst":   {dyntzT, 0, "America/Argentina/Buenos_Aires"},
	"bot":    {tzT, -14400, ""},
	"bra":    {tzT, -10800, ""},
	"brst":   {dtzT, -7200, ""},
	"brt":    {tzT, -10800, ""},
	"cot":    {tzT, -18000, ""},
	"cdt":    {dtzT, -18000, ""},
	"clst":   {dtzT, -10800, ""},
	"clt":    {dyntzT, 0, "America/Santiago"},
	"cst":    {tzT, -21600, ""},
	"edt":    {dtzT, -14400, ""},
	"egst":   {dtzT, 0, ""},
	"egt":    {tzT, -3600, ""},
	"est":    {tzT, -18000, ""},
	"fnt":    {tzT, -7200, ""},
	"fnst":   {dtzT, -3600, ""},
	"gft":    {tzT, -10800, ""},
	"gyt":    {dyntzT, 0, "America/Guyana"},
	"mdt":    {dtzT, -21600, ""},
	"mst":    {tzT, -25200, ""},
	"ndt":    {dtzT, -9000, ""},
	"nft":    {tzT, -12600, ""},
	"nst":    {tzT, -12600, ""},
	"pet":    {tzT, -18000, ""},
	"pdt":    {dtzT, -25200, ""},
	"pmdt":   {dtzT, -7200, ""},
	"pmst":   {tzT, -10800, ""},
	"pst":    {tzT, -28800, ""},
	"pyst":   {dtzT, -10800, ""},
	"pyt":    {dyntzT, 0, "America/Asuncion"},
	"uyst":   {dtzT, -7200, ""},
	"uyt":    {tzT, -10800, ""},
	"vet":    {dyntzT, 0, "America/Caracas"},
	"wgst":   {dtzT, -7200, ""},
	"wgt":    {tzT, -10800, ""},
	"davt":   {dyntzT, 0, "Antarctica/Davis"},
	"ddut":   {tzT, 36000, ""},
	"mawt":   {dyntzT, 0, "Antarctica/Mawson"},
	"aft":    {tzT, 16200, ""},
	"almt":   {tzT, 21600, ""},
	"almst":  {dtzT, 25200, ""},
	"amst":   {dyntzT, 0, "Asia/Yerevan"},
	"amt":    {tzT, -14400, ""},
	"anast":  {dyntzT, 0, "Asia/Anadyr"},
	"anat":   {dyntzT, 0, "Asia/Anadyr"},
	"azst":   {dyntzT, 0, "Asia/Baku"},
	"azt":    {dyntzT, 0, "Asia/Baku"},
	"bdt":    {tzT, 21600, ""},
	"bnt":    {tzT, 28800, ""},
	"bort":   {tzT, 28800, ""},
	"btt":    {tzT, 21600, ""},
	"cct":    {tzT, 28800, ""},
	"gest":   {dyntzT, 0, "Asia/Tbilisi"},
	"get":    {dyntzT, 0, "Asia/Tbilisi"},
	"hkt":    {tzT, 28800, ""},
	"ict":    {tzT, 25200, ""},
	"idt":    {dtzT, 10800, ""},
	"irkst":  {dyntzT, 0, "Asia/Irkutsk"},
	"irkt":   {dyntzT, 0, "Asia/Irkutsk"},
	"irt":    {tzT, 12600, ""},
	"ist":    {tzT, 7200, ""},
	"jayt":   {tzT, 32400, ""},
	"jst":    {tzT, 32400, ""},
	"kdt":    {dtzT, 36000, ""},
	"kgst":   {dtzT, 21600, ""},
	"kgt":    {dyntzT, 0, "Asia/Bishkek"},
	"krast":  {dyntzT, 0, "Asia/Krasnoyarsk"},
	"krat":   {dyntzT, 0, "Asia/Krasnoyarsk"},
	"kst":    {tzT, 32400, ""},
	"lkt":    {dyntzT, 0, "Asia/Colombo"},
	"magst":  {dyntzT, 0, "Asia/Magadan"},
	"magt":   {dyntzT, 0, "Asia/Magadan"},
	"mmt":    {tzT, 23400, ""},
	"myt":    {tzT, 28800, ""},
	"novst":  {dyntzT, 0, "Asia/Novosibirsk"},
	"novt":   {dyntzT, 0, "Asia/Novosibirsk"},
	"npt":    {tzT, 20700, ""},
	"omsst":  {dyntzT, 0, "Asia/Omsk"},
	"omst":   {dyntzT, 0, "Asia/Omsk"},
	"petst":  {dyntzT, 0, "Asia/Kamchatka"},
	"pett":   {dyntzT, 0, "Asia/Kamchatka"},
	"pht":    {tzT, 28800, ""},
	"pkt":    {tzT, 18000, ""},
	"pkst":   {dtzT, 21600, ""},
	"sgt":    {dyntzT, 0, "Asia/Singapore"},
	"tjt":    {tzT, 18000, ""},
	"tmt":    {dyntzT, 0, "Asia/Ashgabat"},
	"ulast":  {dtzT, 32400, ""},
	"ulat":   {dyntzT, 0, "Asia/Ulaanbaatar"},
	"uzst":   {dtzT, 21600, ""},
	"uzt":    {tzT, 18000, ""},
	"vlast":  {dyntzT, 0, "Asia/Vladivostok"},
	"vlat":   {dyntzT, 0, "Asia/Vladivostok"},
	"xjt":    {tzT, 21600, ""},
	"yakst":  {dyntzT, 0, "Asia/Yakutsk"},
	"yakt":   {dyntzT, 0, "Asia/Yakutsk"},
	"yekst":  {dtzT, 21600, ""},
	"yekt":   {dyntzT, 0, "Asia/Yekaterinburg"},
	"adt":    {dtzT, -10800, ""},
	"ast":    {tzT, -14400, ""},
	"azost":  {dtzT, 0, ""},
	"azot":   {tzT, -3600, ""},
	"fkst":   {dyntzT, 0, "Atlantic/Stanley"},
	"fkt":    {dyntzT, 0, "Atlantic/Stanley"},
	"acsst":  {dtzT, 37800, ""},
	"acdt":   {dtzT, 37800, ""},
	"acst":   {tzT, 34200, ""},
	"acwst":  {tzT, 31500, ""},
	"aesst":  {dtzT, 39600, ""},
	"aedt":   {dtzT, 39600, ""},
	"aest":   {tzT, 36000, ""},
	"awsst":  {dtzT, 32400, ""},
	"awst":   {tzT, 28800, ""},
	"cadt":   {dtzT, 37800, ""},
	"cast":   {tzT, 34200, ""},
	"lhdt":   {dyntzT, 0, "Australia/Lord_Howe"},
	"lhst":   {tzT, 37800, ""},
	"ligt":   {tzT, 36000, ""},
	"nzt":    {tzT, 43200, ""},
	"sadt":   {dtzT, 37800, ""},
	"wadt":   {dtzT, 28800, ""},
	"wast":   {tzT, 25200, ""},
	"wdt":    {dtzT, 32400, ""},
	"gmt":    {tzT, 0, ""},
	"uct":    {tzT, 0, ""},
	"ut":     {tzT, 0, ""},
	"utc":    {tzT, 0, ""},
	"z":      {tzT, 0, ""},
	"zulu":   {tzT, 0, ""},
	"bst":    {dtzT, 3600, ""},
	"bdst":   {dtzT, 7200, ""},
	"cest":   {dtzT, 7200, ""},
	"cet":    {tzT, 3600, ""},
	"cetdst": {dtzT, 7200, ""},
	"eest":   {dtzT, 10800, ""},
	"eet":    {tzT, 7200, ""},
	"eetdst": {dtzT, 10800, ""},
	"fet":    {tzT, 10800, ""},
	"mest":   {dtzT, 7200, ""},
	"mesz":   {dtzT, 7200, ""},
	"met":    {tzT, 3600, ""},
	"metdst": {dtzT, 7200, ""},
	"mez":    {tzT, 3600, ""},
	"msd":    {dtzT, 14400, ""},
	"msk":    {dyntzT, 0, "Europe/Moscow"},
	"volt":   {dyntzT, 0, "Europe/Volgograd"},
	"wet":    {tzT, 0, ""},
	"wetdst": {dtzT, 3600, ""},
	"cxt":    {tzT, 25200, ""},
	"iot":    {dyntzT, 0, "Indian/Chagos"},
	"mut":    {tzT, 14400, ""},
	"must":   {dtzT, 18000, ""},
	"mvt":    {tzT, 18000, ""},
	"ret":    {tzT, 14400, ""},
	"sct":    {tzT, 14400, ""},
	"tft":    {tzT, 18000, ""},
	"chadt":  {dtzT, 49500, ""},
	"chast":  {tzT, 45900, ""},
	"chut":   {tzT, 36000, ""},
	"ckt":    {dyntzT, 0, "Pacific/Rarotonga"},
	"easst":  {dyntzT, 0, "Pacific/Easter"},
	"east":   {dyntzT, 0, "Pacific/Easter"},
	"fjst":   {dtzT, 46800, ""},
	"fjt":    {tzT, 43200, ""},
	"galt":   {tzT, -21600, ""},
	"gamt":   {tzT, -32400, ""},
	"gilt":   {tzT, 43200, ""},
	"hst":    {tzT, -36000, ""},
	"kost":   {dyntzT, 0, "Pacific/Kosrae"},
	"lint":   {dyntzT, 0, "Pacific/Kiritimati"},
	"mart":   {tzT, -34200, ""},
	"mht":    {tzT, 43200, ""},
	"mpt":    {tzT, 36000, ""},
	"nut":    {dyntzT, 0, "Pacific/Niue"},
	"nzdt":   {dtzT, 46800, ""},
	"nzst":   {tzT, 43200, ""},
	"pgt":    {tzT, 36000, ""},
	"pont":   {tzT, 39600, ""},
	"pwt":    {tzT, 32400, ""},
	"taht":   {tzT, -36000, ""},
	"tkt":    {dyntzT, 0, "Pacific/Fakaofo"},
	"tot":    {tzT, 46800, ""},
	"trut":   {tzT, 36000, ""},
	"tvt":    {tzT, 43200, ""},
	"vut":    {tzT, 39600, ""},
	"wakt":   {tzT, 43200, ""},
	"wft":    {tzT, 43200, ""},
	"yapt":   {tzT, 36000, ""},
}

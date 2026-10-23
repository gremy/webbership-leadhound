package gates

// hubCities lists, by country, the cities a posting's location most often names: the large
// tech hubs of the United States, Canada, the United Kingdom and Europe, and the largest
// elsewhere. It was chosen by hand from the cities that host large engineering offices or
// recur in ATS location fields, so it is deliberately incomplete; a city it lacks is simply
// not recognised, and the gates then decide as they would without it. Names that are also
// common English words ("Reading", "Bath", "Nice", "Split") or that more often name another
// place ("York", "Victoria", "San José") are left out, and city-states (Singapore, Hong Kong,
// Luxembourg) are read as countries. Spellings with and without diacritics are both listed.
//
// It never holds a candidate's own city: allowed cities come from the private profile only.
var hubCities = map[string][]string{
	"United States": {
		"new york", "new york city", "nyc", "brooklyn", "manhattan", "jersey city", "hoboken",
		"san francisco", "bay area", "san francisco bay area", "silicon valley", "south san francisco",
		"san jose", "palo alto", "mountain view", "menlo park", "sunnyvale", "santa clara", "redwood city",
		"san mateo", "san carlos", "cupertino", "los gatos", "foster city", "burlingame", "oakland",
		"berkeley", "emeryville", "los angeles", "santa monica", "irvine", "san diego", "santa barbara",
		"sacramento", "seattle", "bellevue", "redmond", "kirkland", "portland", "boston", "somerville",
		"austin", "dallas", "houston", "san antonio", "chicago", "denver", "boulder", "atlanta", "miami",
		"tampa", "orlando", "raleigh", "pittsburgh", "philadelphia", "baltimore", "detroit", "ann arbor",
		"minneapolis", "salt lake city", "lehi", "phoenix", "scottsdale", "las vegas", "nashville",
		"charlotte", "columbus", "cincinnati", "indianapolis", "kansas city", "st. louis", "st louis",
		"princeton", "new haven", "providence", "madison", "milwaukee", "omaha", "reston", "herndon",
		"mclean", "huntsville", "boise",
	},
	"Canada": {
		"toronto", "montreal", "montréal", "vancouver", "ottawa", "waterloo", "kitchener", "calgary",
		"edmonton", "quebec city", "québec city", "halifax", "winnipeg",
	},
	"United Kingdom": {
		"london", "greater london", "manchester", "edinburgh", "glasgow", "birmingham", "bristol",
		"cambridge", "oxford", "leeds", "belfast", "cardiff", "newcastle", "liverpool", "sheffield",
		"nottingham", "brighton", "milton keynes", "aberdeen", "dundee", "guildford",
	},
	"Ireland":                {"dublin", "cork", "galway", "limerick"},
	"Germany":                {"berlin", "munich", "münchen", "hamburg", "frankfurt", "frankfurt am main", "cologne", "köln", "stuttgart", "düsseldorf", "dusseldorf", "leipzig", "dresden", "karlsruhe", "nuremberg", "nürnberg", "hanover", "hannover", "bonn", "heidelberg", "darmstadt", "mannheim", "essen", "dortmund", "aachen", "freiburg", "potsdam"},
	"France":                 {"paris", "lyon", "marseille", "toulouse", "bordeaux", "lille", "nantes", "montpellier", "grenoble", "sophia antipolis", "rennes", "strasbourg"},
	"Spain":                  {"madrid", "barcelona", "valencia", "seville", "sevilla", "malaga", "málaga", "bilbao", "zaragoza", "alicante"},
	"Portugal":               {"lisbon", "lisboa", "porto", "braga", "coimbra", "aveiro"},
	"Netherlands":            {"amsterdam", "rotterdam", "utrecht", "eindhoven", "the hague", "den haag", "delft", "leiden", "groningen"},
	"Belgium":                {"brussels", "bruxelles", "antwerp", "ghent", "gent", "leuven"},
	"Luxembourg":             {"luxembourg city"},
	"Italy":                  {"milan", "milano", "rome", "roma", "turin", "torino", "bologna", "florence", "firenze", "naples", "napoli", "genoa"},
	"Austria":                {"vienna", "wien", "graz", "linz", "salzburg", "innsbruck"},
	"Switzerland":            {"zurich", "zürich", "geneva", "genève", "basel", "lausanne", "bern", "zug"},
	"Sweden":                 {"stockholm", "gothenburg", "göteborg", "malmö", "malmo", "uppsala"},
	"Denmark":                {"copenhagen", "aarhus", "odense"},
	"Norway":                 {"oslo", "bergen", "trondheim"},
	"Finland":                {"helsinki", "espoo", "tampere", "oulu"},
	"Iceland":                {"reykjavik", "reykjavík"},
	"Lithuania":              {"vilnius", "kaunas"},
	"Latvia":                 {"riga"},
	"Estonia":                {"tallinn", "tartu"},
	"Poland":                 {"warsaw", "warszawa", "krakow", "kraków", "wroclaw", "wrocław", "gdansk", "gdańsk", "poznan", "poznań", "lodz", "łódź", "katowice"},
	"Czechia":                {"prague", "praha", "brno", "ostrava"},
	"Slovakia":               {"bratislava", "kosice", "košice"},
	"Hungary":                {"budapest", "debrecen"},
	"Romania":                {"bucharest", "bucurești", "bucuresti", "cluj", "cluj-napoca", "iași", "iasi", "brașov", "brasov", "sibiu", "oradea", "constanța", "constanta"},
	"Bulgaria":               {"sofia", "plovdiv", "varna"},
	"Greece":                 {"athens", "thessaloniki"},
	"Croatia":                {"zagreb"},
	"Slovenia":               {"ljubljana"},
	"Serbia":                 {"belgrade", "novi sad"},
	"Moldova":                {"chisinau", "chișinău"},
	"Ukraine":                {"kyiv", "kiev", "lviv", "kharkiv"},
	"Cyprus":                 {"limassol", "nicosia"},
	"Malta":                  {"valletta"},
	"Albania":                {"tirana"},
	"North Macedonia":        {"skopje"},
	"Bosnia and Herzegovina": {"sarajevo"},
	"Turkey":                 {"istanbul", "ankara", "izmir"},
	"Israel":                 {"tel aviv", "jerusalem", "haifa", "herzliya"},
	"United Arab Emirates":   {"dubai", "abu dhabi"},
	"Saudi Arabia":           {"riyadh"},
	"Qatar":                  {"doha"},
	"Egypt":                  {"cairo"},
	"Nigeria":                {"lagos"},
	"Kenya":                  {"nairobi"},
	"South Africa":           {"cape town", "johannesburg"},
	"Morocco":                {"casablanca"},
	"Tunisia":                {"tunis"},
	"India":                  {"bangalore", "bengaluru", "hyderabad", "pune", "mumbai", "chennai", "delhi", "new delhi", "gurgaon", "gurugram", "noida", "kolkata", "ahmedabad"},
	"Pakistan":               {"karachi", "lahore", "islamabad"},
	"Malaysia":               {"kuala lumpur"},
	"Indonesia":              {"jakarta"},
	"Philippines":            {"manila"},
	"Vietnam":                {"ho chi minh city", "hanoi"},
	"Thailand":               {"bangkok"},
	"China":                  {"beijing", "shanghai", "shenzhen", "hangzhou"},
	"Taiwan":                 {"taipei"},
	"Japan":                  {"tokyo", "osaka"},
	"South Korea":            {"seoul"},
	"Australia":              {"sydney", "melbourne", "brisbane", "perth", "adelaide", "canberra"},
	"New Zealand":            {"auckland", "wellington"},
	"Brazil":                 {"são paulo", "sao paulo", "rio de janeiro", "florianópolis", "florianopolis", "belo horizonte"},
	"Argentina":              {"buenos aires"},
	"Chile":                  {"santiago"},
	"Colombia":               {"bogotá", "bogota", "medellín", "medellin"},
	"Peru":                   {"lima"},
	"Uruguay":                {"montevideo"},
	"Mexico":                 {"mexico city", "ciudad de méxico", "guadalajara", "monterrey"},
}

// cityCountry maps a lower-case city to its country.
var cityCountry = func() map[string]string {
	m := map[string]string{}
	for country, cities := range hubCities {
		for _, c := range cities {
			m[c] = country
		}
	}
	return m
}()

// usStates and canadaProvinces map a lower-case state or province to its country.
// "Georgia" is left out: it names a country as often as a US state, and either reading
// would decide a posting on a guess. "GA" after a comma ("Atlanta, GA") still reads as
// the state.
var subdivisions = func() map[string]string {
	m := map[string]string{}
	for _, s := range []string{
		"alabama", "alaska", "arizona", "arkansas", "california", "colorado", "connecticut",
		"delaware", "florida", "hawaii", "idaho", "illinois", "indiana", "iowa", "kansas",
		"kentucky", "louisiana", "maine", "maryland", "massachusetts", "michigan", "minnesota",
		"mississippi", "missouri", "montana", "nebraska", "nevada", "new hampshire", "new jersey",
		"new mexico", "north carolina", "north dakota", "ohio", "oklahoma", "oregon",
		"pennsylvania", "rhode island", "south carolina", "south dakota", "tennessee", "texas",
		"utah", "vermont", "virginia", "washington", "west virginia", "wisconsin", "wyoming",
		"district of columbia",
	} {
		m[s] = "United States"
	}
	for _, p := range []string{
		"ontario", "quebec", "québec", "british columbia", "alberta", "manitoba", "saskatchewan",
		"nova scotia", "new brunswick", "newfoundland and labrador", "prince edward island",
	} {
		m[p] = "Canada"
	}
	return m
}()

// subdivisionCodes are the two-letter US state and Canadian province codes, read only
// right after a comma ("Menlo Park, CA"). "DE" and "NL" are left out: after a city they
// far more often mean Germany and the Netherlands.
var subdivisionCodes = func() map[string]string {
	m := map[string]string{}
	for _, s := range []string{
		"AL", "AK", "AZ", "AR", "CA", "CO", "CT", "FL", "GA", "HI", "ID", "IL", "IN", "IA",
		"KS", "KY", "LA", "ME", "MD", "MA", "MI", "MN", "MS", "MO", "MT", "NE", "NV", "NH", "NJ",
		"NM", "NY", "NC", "ND", "OH", "OK", "OR", "PA", "RI", "SC", "SD", "TN", "TX", "UT", "VT",
		"VA", "WA", "WV", "WI", "WY", "DC",
	} {
		m[s] = "United States"
	}
	for _, p := range []string{"ON", "QC", "BC", "AB", "MB", "SK", "NS", "NB", "PE"} {
		m[p] = "Canada"
	}
	return m
}()

// countryCodes are the ISO codes of the countries whose code is also a state or province
// code, so "Toronto, CA", "Tel Aviv, IL" or "Bangalore, IN" keep their city's country.
var countryCodes = map[string]string{
	"Albania": "AL", "Argentina": "AR", "Azerbaijan": "AZ", "Canada": "CA", "Colombia": "CO",
	"Indonesia": "ID", "Israel": "IL", "India": "IN", "Morocco": "MA", "Moldova": "MD",
	"Montenegro": "ME", "Malta": "MT", "Panama": "PA", "Tunisia": "TN", "Slovakia": "SK",
}

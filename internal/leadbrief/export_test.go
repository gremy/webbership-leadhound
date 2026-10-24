package leadbrief

// LoadExcluding is Load with reserved as the TLDs left out (an empty list means
// leadstore.ReservedTLDs), for the external tests: fixture data lives on reserved TLDs
// only, so they narrow the list to see fixture rows counted.
var LoadExcluding = loadExcluding

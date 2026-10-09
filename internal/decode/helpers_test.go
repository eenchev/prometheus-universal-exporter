package decode

func boolPtr(value bool) *bool { return &value }

// csvDocument is data as the csv decoder once gave its rows, a list of maps
// or of lists (CSVRows.Document), and other data as it is.
func csvDocument(data any) any {
	if rows, ok := data.(*CSVRows); ok {
		return rows.Document()
	}
	return data
}

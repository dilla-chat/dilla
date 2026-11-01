package music

// SkippedTrack is a durably removed queue item and its classified start failure.
// Notices use only the item ID and class; track metadata stays voice scoped.
type SkippedTrack struct {
	Track   Track
	Failure error
}

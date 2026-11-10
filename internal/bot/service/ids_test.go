package service

import "github.com/dilla-chat/dilla/internal/id"

// testID is a fixed id whose last byte is n (it lived with the structural-event tests until Bot v2 task 14 deleted
// them).
func testID(n byte) id.ID { var v id.ID; v[15] = n; return v }

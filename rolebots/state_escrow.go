package rolebots

// EscrowState is a listing being made on the escrow market (escrow_sell.go). Reset on re-plan.
type EscrowState struct {
	// The listing fee is sunk on post, so these fields are bookkeeping only; losing them risks
	// nothing.
	EscrowListTypeID   int64   `json:"escrowListTypeID,omitempty"`
	EscrowListTypeName string  `json:"escrowListTypeName,omitempty"`
	EscrowListPrice    float64 `json:"escrowListPrice,omitempty"`
	EscrowListStation  int64   `json:"escrowListStation,omitempty"`
}

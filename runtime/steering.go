package runtime

import "context"

// The marker reaches the saved user message together with its steering text.
// This closes the placement/acknowledgement crash window without treating
// equal text from two distinct submissions as the same input.
func steerText(item Submission) string {
	return "[bonnie submission " + item.ID + "]\n" + item.Input.Text
}

func (r *Runner) steerPlaced(ctx context.Context, item Submission) (bool, error) {
	recs, err := r.journal.Replay(ctx, item.RunID)
	if err != nil {
		return false, err
	}
	for _, rec := range recs {
		if rec.Kind != RecordMessage || rec.Role != "user" {
			continue
		}
		msg, err := decodeMessage(rec)
		if err != nil {
			return false, err
		}
		if messageText(msg) == steerText(item) {
			return true, nil
		}
	}
	return false, nil
}

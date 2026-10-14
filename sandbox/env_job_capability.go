package sandbox

import "context"

type envOutputSandbox struct{ *envSandbox }

func (s *envOutputSandbox) ReadJobOutput(ctx context.Context, id string, limit int64) (string, string, bool, error) {
	return s.Sandbox.(JobOutputReader).ReadJobOutput(ctx, id, limit)
}

type envOutputDeleterSandbox struct{ *envOutputSandbox }

func (s *envOutputDeleterSandbox) Delete(ctx context.Context) error {
	return s.Sandbox.(Deleter).Delete(ctx)
}

type envJobDeleterSandbox struct{ *envJobSandbox }

func (s *envJobDeleterSandbox) Delete(ctx context.Context) error {
	return s.Sandbox.(Deleter).Delete(ctx)
}

type envJobReaderSandbox struct{ *envJobSandbox }

func (s *envJobReaderSandbox) ReadJobOutput(ctx context.Context, id string, limit int64) (string, string, bool, error) {
	return s.jobs.(JobOutputReader).ReadJobOutput(ctx, id, limit)
}

type envJobDeleterReaderSandbox struct{ *envJobSandbox }

func (s *envJobDeleterReaderSandbox) Delete(ctx context.Context) error {
	return s.Sandbox.(Deleter).Delete(ctx)
}
func (s *envJobDeleterReaderSandbox) ReadJobOutput(ctx context.Context, id string, limit int64) (string, string, bool, error) {
	return s.jobs.(JobOutputReader).ReadJobOutput(ctx, id, limit)
}

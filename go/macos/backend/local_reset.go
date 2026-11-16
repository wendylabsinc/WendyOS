package backend

import "github.com/wendylabsinc/wendy/go/internal/cli/commands"

func (b *Backend) PrepareLocalReset(session uint64, account, organization, device string) (commands.DesktopLocalResetPlan, error) {
	ctx, done := b.begin()
	defer done()
	conn, err := b.connection(session)
	if err != nil {
		return commands.DesktopLocalResetPlan{}, err
	}
	return commands.DesktopPrepareLocalReset(ctx, conn, account, organization, device)
}
func (b *Backend) ResetLocalEnrollment(session uint64, account, organization, device, principal, fingerprint string) (bool, error) {
	ctx, done := b.begin()
	defer done()
	conn, err := b.connection(session)
	if err != nil {
		return false, err
	}
	return commands.DesktopResetLocalEnrollment(ctx, conn, account, organization, device, commands.DesktopLocalResetPlan{Principal: principal, Fingerprint: fingerprint})
}

# System configuration assets

`etc/jobman-control/` contains inert, safe packaging assets. Native packages
install the systemd unit and an example environment file but do not enable or
start the service.

Copy the example to an untracked root-owned mode-0600 file and replace every
placeholder through the deployment's secret-management process. Review
`docs/DEPLOYMENT.md` before enabling the unit.

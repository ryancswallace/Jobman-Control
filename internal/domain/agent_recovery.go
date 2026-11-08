package domain

// AgentRecovery preserves an enrolled identity using fresh enrollment authority.
// Certificate material is public; the agent retains its new private key.
type AgentRecovery struct {
	AgentID                   string
	TargetGenerationID        string
	PreviousCertificateSerial string
	ExecutionUser             string
	RequestDigest             string
	Certificate               AgentCertificate
	CertificatePEM            string
	CACertificatePEM          string
}

// AgentRecoveryResult retains the exact issued credential for bounded retries.
type AgentRecoveryResult struct {
	Session          AgentSession
	Certificate      AgentCertificate
	CertificatePEM   string
	CACertificatePEM string
}

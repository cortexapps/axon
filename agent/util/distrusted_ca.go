package util

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
)

// SHA-256 of the SubjectPublicKeyInfo of CA certificates that must never be
// trusted. Matching on the key catches every certificate for that key,
// including cross-signed copies under another issuer.
var distrustedCASPKIs = map[string]bool{
	// eMudhra subordinate CAs whose private keys are compromised.
	"ebed6475c2c2d80fcb199f3fe8dd6e6b3ce60b77eb15d4e42c3dec0c6f5b5105": true, // EM DV TLS CA - G3B
	"49dd353dc2b919a3770645117d0e60c16115e7d1bc259eefe734b00ea434fe5f": true, // EM DV TLS CA - G3C
	"e0254c7fcdc265e2f0f8a428dabb75ab4ae039b4872d62f5ee57f0877d5a8a41": true, // EM EV TLS CA - G3B
	"5c0660a2b164910d8ff589c18e14552e9e0ec237dcefe2cb736d41a4811f81a0": true, // EM EV TLS CA - G3C
	"43bffa19d6ef651451f6841e019931e96f6f2eaeff28ab4b6f1ce4576de2717b": true, // EM OV TLS CA - G3B
	"63ecefee431d49523bc1136c4e72d0206a477086caf606cd5cf025686c109cf8": true, // EM OV TLS CA - G3C
	"ec4ffa1ad0ac88081314b7aaffef6a91f97cd57291bb5ba686c5415f5eb4f6b6": true, // EM OV CS CA - G3B
	"92c063d67e8f14db71d288e11266ffdaf69cbf115f7bab14f3bc2f68d25e0e30": true, // EM OV CS CA - G3C
	// The two roots that signed them. Neither is trusted for TLS by browsers.
	"8d417db2dd8bf5e3084d1e3f196d583849d81bdd4c00c70b9d39369e96b8c782": true, // emSign ECC Root CA - G3
	"bd153ed7b0434f6886b17bce8bbe84ed340c7132d702a8f4fa318f756ecbd6f3": true, // AAA Certificate Services
}

// FilterDistrustedCAs returns pemData without the certificates whose key is
// distrusted, plus the subjects of the certificates it removed. Blocks that
// are not parseable certificates are kept unchanged, so an unusual bundle is
// never made worse.
func FilterDistrustedCAs(pemData []byte) ([]byte, []string) {
	var kept bytes.Buffer
	var removed []string
	rest := pemData
	for {
		block, next := pem.Decode(rest)
		if block == nil {
			kept.Write(rest)
			break
		}
		raw := rest[:len(rest)-len(next)]
		rest = next

		if block.Type == "CERTIFICATE" {
			if cert, err := x509.ParseCertificate(block.Bytes); err == nil {
				sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
				if distrustedCASPKIs[hex.EncodeToString(sum[:])] {
					removed = append(removed, cert.Subject.String())
					continue
				}
			}
		}
		kept.Write(raw)
	}
	return kept.Bytes(), removed
}

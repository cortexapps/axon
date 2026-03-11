#!/bin/bash
# Generate TLS certificates for the relay E2E test
# This creates a broker server cert signed by the mitmproxy CA

set -e

CERT_DIR="$(dirname "$0")/.mitmproxy"

# Check if mitmproxy CA exists (created by mitmproxy on first run)
if [ ! -f "$CERT_DIR/mitmproxy-ca.pem" ]; then
    echo "Error: mitmproxy CA not found at $CERT_DIR/mitmproxy-ca.pem"
    echo "Run mitmproxy once to generate the CA, or copy existing certs."
    exit 1
fi

cd "$CERT_DIR"

# Always create combined CA bundle (needed even if broker certs exist)
# This must be done on each machine since system CAs differ
echo "Creating combined CA bundle..."
if [ -f /etc/ssl/certs/ca-certificates.crt ]; then
    # Linux (Debian/Ubuntu)
    cat /etc/ssl/certs/ca-certificates.crt mitmproxy-ca-cert.pem > combined-ca-bundle.crt
elif [ -f /etc/pki/tls/certs/ca-bundle.crt ]; then
    # Linux (RHEL/CentOS)
    cat /etc/pki/tls/certs/ca-bundle.crt mitmproxy-ca-cert.pem > combined-ca-bundle.crt
elif [ -f /etc/ssl/cert.pem ]; then
    # macOS
    cat /etc/ssl/cert.pem mitmproxy-ca-cert.pem > combined-ca-bundle.crt
else
    echo "Warning: Could not find system CA bundle, using only mitmproxy CA"
    cp mitmproxy-ca-cert.pem combined-ca-bundle.crt
fi
echo "Combined CA bundle: $(grep -c 'BEGIN CERTIFICATE' combined-ca-bundle.crt) certificates"

# Check if broker cert already exists
if [ -f "$CERT_DIR/broker-server-cert.pem" ] && [ -f "$CERT_DIR/broker-server-key.pem" ]; then
    echo "Broker certificates already exist, skipping generation"
    exit 0
fi

echo "Generating broker server certificates..."

# Extract mitmproxy CA key
openssl pkey -in mitmproxy-ca.pem -out mitmproxy-ca-key.pem 2>/dev/null

# Generate broker server key
openssl genrsa -out broker-server-key.pem 2048 2>/dev/null

# Create broker server CSR
openssl req -new -key broker-server-key.pem -out broker-server.csr \
    -subj "/CN=snyk-broker-tls/O=test" 2>/dev/null

# Create extensions file with SAN
cat > broker-server-ext.cnf << 'EOF'
subjectAltName = DNS:snyk-broker-tls, DNS:snyk-broker, DNS:localhost
EOF

# Sign with mitmproxy CA
openssl x509 -req -in broker-server.csr -CA mitmproxy-ca-cert.pem -CAkey mitmproxy-ca-key.pem \
    -CAcreateserial -out broker-server-cert.pem -days 3650 -extfile broker-server-ext.cnf 2>/dev/null

# Clean up temp files
rm -f broker-server.csr broker-server-ext.cnf mitmproxy-ca-key.pem

echo "Certificates generated successfully:"
openssl x509 -in broker-server-cert.pem -noout -subject -issuer

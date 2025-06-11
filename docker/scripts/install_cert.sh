#! /bin/sh

#
# Use this in your Dockerfile to install custom certs.
# Works with CRT or PEM files.
#
# COPY .certs/my-ca.crt /certs/
# RUN /agent/scripts/install_cert.sh /certs/my-ca.crt
#

cert_path=$1
if [ -z "$cert_path" ]; then
  echo "Usage: $0 <path_to_certificate>"
  exit 1
fi

echo "Installing certificate from $cert_path"
apt-get install -yq  ca-certificates openssl

# if it's a pem file, convert it to crt
if [ "${cert_path##*.}" = "pem" ]; then
  echo "Converting PEM to CRT"
  openssl x509 -in "$cert_path" -out "${cert_path%.pem}.crt" -outform PEM
  cert_path="${cert_path%.pem}.crt"
fi


if [ "${cert_path##*.}" != "crt" ]; then
  echo "Error: Certificate file must be in .crt format"
  exit 1
fi

echo "Copying $cert_path to /usr/local/share/ca-certificates/"
cp "$cert_path" /usr/local/share/ca-certificates/
update-ca-certificates >&2

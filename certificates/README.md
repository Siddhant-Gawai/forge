# Supabase database certificate

`supabase-ca.crt` is the public CA certificate downloaded from:

https://supabase-downloads.s3-ap-southeast-1.amazonaws.com/prod/ssl/prod-ca-2021.crt

It is embedded in the Go executable and added to the TLS root pool only for Supabase database hostnames. Hostname verification stays enabled. It contains no private key or project credentials. `DATABASE_SSL_ROOT_CERT` can supply a different provider CA.

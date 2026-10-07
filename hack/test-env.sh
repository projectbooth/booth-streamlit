#!/usr/bin/env sh
# Prints the environment variables that point the test suite at the container from
# docker-compose.emulators.yml. Usage: eval "$(sh hack/test-env.sh)"
cat <<'VARS'
export BOOTH_TEST_POSTGRES_DSN=postgres://booth:booth-test@127.0.0.1:15436/booth_streamlit_test?sslmode=disable
VARS

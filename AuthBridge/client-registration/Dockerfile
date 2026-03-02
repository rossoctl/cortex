# Use minimal Python base image
FROM python:3.12-slim

# Create non-root user with UID/GID 1337 (matches Envoy proxy and spiffe-helper)
# IMPORTANT: These values MUST match ClientRegistrationUID/GID in
# kagenti-webhook/internal/webhook/injector/container_builder.go
# The webhook injects this container with runAsUser/runAsGroup set to those constants.
RUN groupadd -g 1337 appuser && useradd -u 1337 -g appuser appuser

# Set work directory
WORKDIR /app

# Copy your script
COPY client_registration.py .

# Install dependencies
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

# Ensure non-root user owns application directory
RUN chown -R 1337:1337 /app

# Switch to non-root user
USER 1337:1337

# Register client
CMD ["python", "client_registration.py"]
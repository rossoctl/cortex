# Kagenti Extensions

This repository contains extension projects for the Kagenti ecosystem

## Projects

- [kagenti-webhook](./kagenti-webhook/) - Admission webhook for Kubernetes workloads (Deployments, StatefulSets, etc.) with Envoy proxy and optional SPIFFE/SPIRE integration. *(Legacy support for MCPServer and Agent CRs - deprecated)*
- [AuthBridge](./AuthBridge/) - Collection of Identity components to demonstrate a complete end-to-end authentication flow with [SPIFFE/SPIRE](https://spiffe.io) integration
  - [AuthProxy](./AuthBridge/AuthProxy/) - AuthProxy is a **JWT validation and token exchange proxy** for Kubernetes workloads. It enables secure service-to-service communication by intercepting and validating incoming tokens and transparently exchanging them for tokens with the correct audience for downstream services.
  - [Keycloak client-registration](./AuthBridge/client-registration/) - Keycloak Client Registration is an **automated OAuth2/OIDC client provisioning** tool for Kubernetes workloads. It automatically registers pods as Keycloak clients, eliminating the need for manual client configuration and static credentials.
    

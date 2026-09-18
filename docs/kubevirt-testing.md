# KubeVirt VM Testing

KubeVirt VMIs in UDN-enabled namespaces **must** use `bridge` binding, not `masquerade`.

The `masquerade` binding installs nftables rules in the virt-launcher pod that drop inbound traffic on UDN interfaces (`ovn-udn1`). This blocks both IPv4 and IPv6 connectivity to the VM from other pods and external hosts.

VM spec (relevant sections):

```yaml
spec:
  domain:
    devices:
      interfaces:
        - name: default
          bridge: {}
  networks:
    - name: default
      pod: {}
```

| Binding | Inbound to VM (UDN) | Outbound from VM |
|---------|---------------------|------------------|
| masquerade | Blocked by nftables | Works |
| bridge | Works | Works |

For a VM migration test using Migration Toolkit for Virtualization (MTV) see [test-mtv-cudn.md](test-mtv-cudn.md).

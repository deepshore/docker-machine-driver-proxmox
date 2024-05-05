# docker-machine-driver-proxmox
## ansible playbook for vm template

This playbook is based on learnings from [technotim.live](https://docs.technotim.live/posts/cloud-init-cloud-image/) and will create a minimal vm template from cloud-init images.

Qemu Guest Agent installation will also be part of this as the docker-machine driver needs the ip of the vm to operate.

## create inventory

create a file named inventory.yaml and add your info

```
all:
  hosts:
    proxmox-myhost:
      ansible_host: 127.0.0.1
      ansible_connection: ssh
      ansible_python_interpreter: /usr/bin/python3
      proxmox_vm_sshkeys: your public key
      proxmox_vm_ipconfig: ip=dhcp
      storage_name: local-lvm
```

## run 

`ansible-playbook --inventory-file inventory.yaml -u root -k playbook.yaml`

if you have multiple proxmox nodes in your inventory file then limit to the first:

`ansible-playbook --inventory-file inventory.yaml -u root -k playbook.yaml --limit firstnode`


## clean old templates

this should be applied to all proxmox nodes to clean templates according to `image_prefix` defined in vars.

`ansible-playbook --inventory-file inventory.yaml -u root -k playclean.yaml`
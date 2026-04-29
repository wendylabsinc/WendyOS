# Packer template for the WendyOS CI runner AMI.
#
# Bakes the Yocto host prerequisites (apt packages, locales) into a custom
# Ubuntu 24.04 AMI so the GitHub Actions build matrix can skip the per-run
# `docker build` + apt-get cycle entirely. Optionally pre-clones the upstream
# Yocto layer repositories at the SRCREVs pinned by scripts/upstream-repos.env
# into /opt/wendyos-cache/repos so bootstrap.sh's clone_repos sees them as
# already-checked-out and falls through to a cheap `git fetch + git checkout`.
#
# Build:
#   cd ci/packer
#   packer init .
#   packer build wendyos-builder.pkr.hcl
#
# CI invokes this from .github/workflows/build-ami.yml on changes to the
# package list or pinned SRCREVs (and on a weekly cron for security updates).

packer {
  required_version = ">= 1.10.0"
  required_plugins {
    amazon = {
      source  = "github.com/hashicorp/amazon"
      version = ">= 1.3.0"
    }
  }
}

variable "aws_region" {
  type        = string
  description = "AWS region to build the AMI in. Must match the region RunsOn launches runners in."
}

variable "instance_type" {
  type        = string
  description = "Builder EC2 instance type. Anything modest works; the AMI is reused."
  default     = "c6i.large"
}

variable "subnet_id" {
  type        = string
  description = "Optional subnet to launch the builder in. Empty = Packer picks a default VPC subnet."
  default     = ""
}

variable "vpc_id" {
  type        = string
  description = "Optional VPC. Empty = Packer picks the default VPC."
  default     = ""
}

variable "ami_name_prefix" {
  type        = string
  description = "Prefix for the produced AMI name. The build appends a UTC timestamp."
  default     = "wendyos-builder"
}

variable "extra_tags" {
  type        = map(string)
  description = "Tags to attach to the AMI and snapshots in addition to the defaults."
  default     = {}
}

locals {
  timestamp = formatdate("YYYY-MM-DD-hhmmss", timestamp())
  ami_name  = "${var.ami_name_prefix}-${local.timestamp}"

  base_tags = {
    Name        = local.ami_name
    Project     = "wendyos"
    Component   = "ci-builder"
    BuiltBy     = "packer"
    "runs-on-image" = var.ami_name_prefix
  }

  tags = merge(local.base_tags, var.extra_tags)
}

# Latest Canonical-published Ubuntu 24.04 LTS (Noble) amd64 AMI.
source "amazon-ebs" "wendyos_builder" {
  region        = var.aws_region
  instance_type = var.instance_type
  subnet_id     = var.subnet_id == "" ? null : var.subnet_id
  vpc_id        = var.vpc_id == "" ? null : var.vpc_id

  source_ami_filter {
    filters = {
      name                = "ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*"
      root-device-type    = "ebs"
      virtualization-type = "hvm"
      architecture        = "x86_64"
    }
    owners      = ["099720109477"] # Canonical
    most_recent = true
  }

  ssh_username = "ubuntu"

  ami_name        = local.ami_name
  # AWS rejects non-ASCII in this field; keep it plain ASCII.
  ami_description = "WendyOS Yocto build runner - Ubuntu 24.04 + Yocto deps (Scarthgap)"
  ami_virtualization_type = "hvm"

  # Plenty of headroom for the bake-time repo prefetch and apt cache.
  launch_block_device_mappings {
    device_name           = "/dev/sda1"
    volume_size           = 30
    volume_type           = "gp3"
    delete_on_termination = true
    encrypted             = true
  }

  tags          = local.tags
  snapshot_tags = local.tags
  run_tags      = local.tags
  run_volume_tags = local.tags
}

build {
  name    = "wendyos-builder"
  sources = ["source.amazon-ebs.wendyos_builder"]

  # Wait until cloud-init has finished so apt locks are free and the network is up.
  provisioner "shell" {
    inline = [
      "cloud-init status --wait || true",
    ]
  }

  # Stage the shared package install script and run it as root.
  provisioner "file" {
    source      = "${path.root}/../../scripts/install-build-deps.sh"
    destination = "/tmp/install-build-deps.sh"
  }

  provisioner "shell" {
    inline = [
      "chmod +x /tmp/install-build-deps.sh",
      "sudo /tmp/install-build-deps.sh",
      "rm -f /tmp/install-build-deps.sh",
    ]
  }

  # Pre-clone the upstream Yocto layer repos at the pinned SRCREVs.
  # bootstrap.sh treats already-existing checkouts as fetch + checkout, so a
  # slightly stale AMI still works — it just runs `git fetch` instead of a
  # full clone.
  provisioner "file" {
    source      = "${path.root}/../../scripts/upstream-repos.env"
    destination = "/tmp/upstream-repos.env"
  }

  provisioner "file" {
    source      = "${path.root}/prefetch-upstream-repos.sh"
    destination = "/tmp/prefetch-upstream-repos.sh"
  }

  provisioner "shell" {
    inline = [
      "chmod +x /tmp/prefetch-upstream-repos.sh",
      "sudo /tmp/prefetch-upstream-repos.sh /opt/wendyos-cache/repos /tmp/upstream-repos.env",
      "rm -f /tmp/prefetch-upstream-repos.sh /tmp/upstream-repos.env",
    ]
  }

  # Final cleanup so the AMI snapshot is as small as possible.
  provisioner "shell" {
    inline = [
      "sudo apt-get -qy autoremove --purge",
      "sudo apt-get -qy clean",
      "sudo rm -rf /var/lib/apt/lists/* /var/log/apt /var/log/cloud-init*.log",
      "sudo journalctl --rotate --vacuum-time=1s || true",
      "sudo cloud-init clean --logs --seed || true",
    ]
  }
}

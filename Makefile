# WendyOS Build System for NVIDIA Jetson Orin Nano
# ================================================
#
# Usage:
#   make help        - Show this help message
#   make setup       - Bootstrap the build environment (first time setup)
#   make build       - Build the complete WendyOS image
#   make shell       - Open interactive shell in build container
#
# For macOS users: Ensure Docker Desktop is running with sufficient resources
# (8GB+ RAM, 4+ CPUs, 150GB+ disk recommended)
#
# Note: On macOS, build artifacts are stored in Docker volumes (case-sensitive)
# rather than the host filesystem to work around macOS case-insensitivity.

.PHONY: help setup bootstrap docker-create docker-run docker-remove shell build build-sdk clean distclean volumes-create volumes-remove deploy flash-to-external _check-machine _check-setup _ensure-volumes

# Configuration
SHELL := /bin/bash
IMAGE_NAME := wendyos
DOCKER_REPO := wendyos-build
DOCKER_TAG := scarthgap
DOCKER_USER := dev
DOCKER_WORKDIR := /home/$(DOCKER_USER)/$(IMAGE_NAME)
BUILD_DIR := build
IMAGE_TARGET ?= wendyos-image

# Flash configuration
FLASH_DEVICE ?=
FLASH_IMAGE_SIZE ?= 64G
FLASH_CONFIRM ?=

# Directories (relative to where Makefile is located)
MAKEFILE_DIR := $(shell dirname $(realpath $(firstword $(MAKEFILE_LIST))))
PROJECT_DIR := $(shell dirname $(MAKEFILE_DIR))
DOCKER_DIR := $(PROJECT_DIR)/docker

# Docker volumes for macOS (case-sensitive storage)
VOLUME_BUILD := wendyos-build-tmp
VOLUME_SSTATE := wendyos-sstate-cache
VOLUME_DOWNLOADS := wendyos-downloads
VOLUME_CACHE := wendyos-build-cache

# Colors for output
CYAN := \033[0;36m
GREEN := \033[0;32m
YELLOW := \033[0;33m
RED := \033[0;31m
NC := \033[0m # No Color

# Default target
.DEFAULT_GOAL := help

#
# Help
#
help:
	@printf "\n"
	@printf "$(CYAN)WendyOS Build System$(NC)\n"
	@printf "$(CYAN)====================$(NC)\n"
	@printf "\n"
	@printf "$(GREEN)Setup Commands:$(NC)\n"
	@printf "  make setup          - First-time setup: clone repos, create Docker image\n"
	@printf "  make docker-create  - Create/rebuild the Docker build image\n"
	@printf "  make docker-remove  - Remove the Docker build image\n"
	@printf "\n"
	@printf "$(GREEN)Build Commands:$(NC)\n"
	@printf "  make build          - Build the complete WendyOS image ($(IMAGE_TARGET))\n"
	@printf "  make build-sdk      - Build the SDK for application development\n"
	@printf "  make shell          - Open interactive shell in build container\n"
	@printf "  make deploy         - Copy tegraflash tarball to host (macOS only)\n"
	@printf "\n"
	@printf "$(GREEN)Flash Commands:$(NC)\n"
	@printf "  make flash-to-external - Interactive: create .img and flash to external drive\n"
	@printf "\n"
	@printf "$(GREEN)Clean Commands:$(NC)\n"
	@printf "  make clean          - Remove build artifacts (keeps downloads/sstate)\n"
	@printf "  make distclean      - Remove everything (downloads, sstate, build)\n"
	@printf "\n"
	@printf "$(GREEN)macOS Volume Commands:$(NC)\n"
	@printf "  make volumes-create - Create Docker volumes for case-sensitive storage\n"
	@printf "  make volumes-remove - Remove Docker volumes (deletes all build data)\n"
	@printf "\n"
	@printf "$(GREEN)Configuration:$(NC)\n"
	@printf "  MACHINE=$(MACHINE)\n"
	@printf "  IMAGE_TARGET=$(IMAGE_TARGET)\n"
	@printf "  FLASH_IMAGE_SIZE=$(FLASH_IMAGE_SIZE)  (must match WENDYOS_FLASH_IMAGE_SIZE)\n"
	@printf "  FLASH_DEVICE=        (e.g., /dev/disk4)\n"
	@printf "  FLASH_CONFIRM=       (set to 'yes' for non-interactive mode)\n"
	@printf "\n"
	@printf "$(YELLOW)Examples:$(NC)\n"
	@printf "  make setup                                    # First time setup\n"
	@printf "  make build                                    # Build default image\n"
	@printf "  make build MACHINE=jetson-orin-nano-devkit-wendyos  # Build for SD card\n"
	@printf "  make build MACHINE=jetson-agx-orin-devkit-nvme-wendyos  # Build for AGX Orin 64GB\n"
	@printf "  make build MACHINE=raspberrypi5-wendyos       # Build for RPi5\n"
	@printf "  make shell                                    # Interactive development\n"
	@printf "  make flash-to-external                        # Interactive flash\n"
	@printf "  make flash-to-external FLASH_DEVICE=/dev/disk4 FLASH_CONFIRM=yes  # Non-interactive\n"
	@printf "\n"
	@if [ "$$(uname)" = "Darwin" ]; then \
		printf "$(YELLOW)macOS Note:$(NC) Build artifacts stored in Docker volumes (case-sensitive)\n"; \
		printf "            Use 'make deploy' to copy tegraflash tarball after build.\n\n"; \
	fi

#
# Setup / Bootstrap
#
setup: bootstrap
	@printf "\n"
	@printf "$(GREEN)Setup complete!$(NC)\n"
	@printf "\n"
	@printf "Next steps:\n"
	@printf "  1. (Optional) Edit $(PROJECT_DIR)/build/conf/local.conf\n"
	@printf "  2. Run: make build\n"
	@printf "\n"

bootstrap:
	@printf "$(CYAN)Running bootstrap...$(NC)\n"
	@cd $(PROJECT_DIR) && $(MAKEFILE_DIR)/bootstrap.sh

#
# Docker Management
#
docker-create:
	@printf "$(CYAN)Creating Docker image...$(NC)\n"
	@if [ -d "$(DOCKER_DIR)" ]; then \
		cd $(DOCKER_DIR) && ./docker-util.sh create; \
	else \
		printf "$(RED)Error: Docker directory not found. Run 'make setup' first.$(NC)\n"; \
		exit 1; \
	fi

docker-remove:
	@printf "$(CYAN)Removing Docker image...$(NC)\n"
	@cd $(DOCKER_DIR) && ./docker-util.sh remove

#
# Interactive Shell
#
shell:
	@printf "$(CYAN)Starting interactive build shell...$(NC)\n"
	@if [ ! -d "$(DOCKER_DIR)" ]; then \
		printf "$(RED)Error: Docker directory not found. Run 'make setup' first.$(NC)\n"; \
		exit 1; \
	fi
	@printf "\n"
	@printf "$(YELLOW)Inside the container, run:$(NC)\n"
	@printf "  cd ./$(IMAGE_NAME)\n"
	@printf "  . ./repos/poky/oe-init-build-env build\n"
	@printf "  bitbake $(IMAGE_TARGET)\n"
	@printf "\n"
	@cd $(DOCKER_DIR) && ./docker-util.sh run

#
# Build Commands
#
build: _check-machine _check-setup _ensure-volumes
	@printf "$(CYAN)Building $(IMAGE_TARGET) for $(MACHINE)...$(NC)\n"
	@printf "$(YELLOW)This may take several hours on first build.$(NC)\n"
	@printf "\n"
	@if [ "$$(uname)" = "Darwin" ]; then \
		docker run \
			--rm -t \
			--privileged \
			-e "TERM=xterm-256color" \
			-e "LANG=C.UTF-8" \
			-v $(PROJECT_DIR):$(DOCKER_WORKDIR) \
			-v $(VOLUME_BUILD):$(DOCKER_WORKDIR)/build/tmp \
			-v $(VOLUME_SSTATE):$(DOCKER_WORKDIR)/sstate-cache \
			-v $(VOLUME_DOWNLOADS):$(DOCKER_WORKDIR)/downloads \
			-v $(VOLUME_CACHE):$(DOCKER_WORKDIR)/build/cache \
			$(DOCKER_REPO):$(DOCKER_TAG) \
			/bin/bash -c '\
				cd $(DOCKER_WORKDIR) && \
				source ./repos/poky/oe-init-build-env $(BUILD_DIR) && \
				BB_ENV_PASSTHROUGH_ADDITIONS="MACHINE" MACHINE=$(MACHINE) bitbake $(IMAGE_TARGET) \
			'; \
	else \
		cd $(DOCKER_DIR) && docker run \
			--rm \
			-v /tmp:/tmp \
			--network host \
			--privileged \
			-e "TERM=xterm-256color" \
			-e "LANG=C.UTF-8" \
			-v $(PROJECT_DIR):$(DOCKER_WORKDIR) \
			$(DOCKER_REPO):$(DOCKER_TAG) \
			/bin/bash -c '\
				cd $(DOCKER_WORKDIR) && \
				source ./repos/poky/oe-init-build-env $(BUILD_DIR) && \
				BB_ENV_PASSTHROUGH_ADDITIONS="MACHINE" MACHINE=$(MACHINE) bitbake $(IMAGE_TARGET) \
			'; \
	fi
	@printf "\n"
	@printf "$(GREEN)Build complete!$(NC)\n"
	@if [ "$$(uname)" = "Darwin" ]; then \
		printf "Run 'make deploy' to copy tegraflash tarball, or 'make flash-to-external' to flash.\n"; \
	else \
		printf "Image location: $(PROJECT_DIR)/build/tmp/deploy/images/$(MACHINE)/\n"; \
	fi

build-sdk: _check-machine _check-setup _ensure-volumes
	@printf "$(CYAN)Building SDK for $(MACHINE)...$(NC)\n"
	@if [ "$$(uname)" = "Darwin" ]; then \
		docker run \
			--rm -t \
			--privileged \
			-e "TERM=xterm-256color" \
			-e "LANG=C.UTF-8" \
			-v $(PROJECT_DIR):$(DOCKER_WORKDIR) \
			-v $(VOLUME_BUILD):$(DOCKER_WORKDIR)/build/tmp \
			-v $(VOLUME_SSTATE):$(DOCKER_WORKDIR)/sstate-cache \
			-v $(VOLUME_DOWNLOADS):$(DOCKER_WORKDIR)/downloads \
			-v $(VOLUME_CACHE):$(DOCKER_WORKDIR)/build/cache \
			$(DOCKER_REPO):$(DOCKER_TAG) \
			/bin/bash -c '\
				cd $(DOCKER_WORKDIR) && \
				source ./repos/poky/oe-init-build-env $(BUILD_DIR) && \
				BB_ENV_PASSTHROUGH_ADDITIONS="MACHINE" MACHINE=$(MACHINE) bitbake $(IMAGE_TARGET) -c populate_sdk \
			'; \
	else \
		cd $(DOCKER_DIR) && docker run \
			--rm \
			-v /tmp/.X11-unix:/tmp/.X11-unix \
			--network host \
			--privileged \
			-e "TERM=xterm-256color" \
			-e "LANG=C.UTF-8" \
			-v $(PROJECT_DIR):$(DOCKER_WORKDIR) \
			$(DOCKER_REPO):$(DOCKER_TAG) \
			/bin/bash -c '\
				cd $(DOCKER_WORKDIR) && \
				source ./repos/poky/oe-init-build-env $(BUILD_DIR) && \
				BB_ENV_PASSTHROUGH_ADDITIONS="MACHINE" MACHINE=$(MACHINE) bitbake $(IMAGE_TARGET) -c populate_sdk \
			'; \
	fi
	@printf "\n"
	@printf "$(GREEN)SDK build complete!$(NC)\n"

#
# Clean Commands
#
clean:
	@printf "$(CYAN)Cleaning build artifacts...$(NC)\n"
	@if [ -d "$(PROJECT_DIR)/build/tmp" ]; then \
		rm -rf $(PROJECT_DIR)/build/tmp; \
		printf "Removed build/tmp\n"; \
	fi
	@if [ -d "$(PROJECT_DIR)/build/cache" ]; then \
		rm -rf $(PROJECT_DIR)/build/cache; \
		printf "Removed build/cache\n"; \
	fi
	@printf "$(GREEN)Clean complete.$(NC)\n"
	@printf "Note: downloads/ and sstate-cache/ preserved for faster rebuilds.\n"

distclean:
	@printf "$(RED)WARNING: This will remove ALL build artifacts including downloads and sstate-cache.$(NC)\n"
	@printf "This cannot be undone and will require re-downloading all sources.\n"
	@read -p "Are you sure? [y/N] " confirm && \
		if [ "$$confirm" = "y" ] || [ "$$confirm" = "Y" ]; then \
			rm -rf $(PROJECT_DIR)/build $(PROJECT_DIR)/downloads $(PROJECT_DIR)/sstate-cache; \
			if [ "$$(uname)" = "Darwin" ]; then \
				docker volume rm $(VOLUME_BUILD) $(VOLUME_SSTATE) $(VOLUME_DOWNLOADS) $(VOLUME_CACHE) 2>/dev/null || true; \
				printf "  Removed Docker volumes.\n"; \
			fi; \
			printf "$(GREEN)Distclean complete.$(NC)\n"; \
		else \
			printf "Cancelled.\n"; \
		fi

#
# macOS Volume Management
#
volumes-create:
	@if [ "$$(uname)" != "Darwin" ]; then \
		printf "$(YELLOW)Volumes only needed on macOS. Skipping.$(NC)\n"; \
		exit 0; \
	fi
	@printf "$(CYAN)Creating Docker volumes for case-sensitive storage...$(NC)\n"
	@docker volume create $(VOLUME_BUILD) >/dev/null && printf "  Created $(VOLUME_BUILD)\n"
	@docker volume create $(VOLUME_SSTATE) >/dev/null && printf "  Created $(VOLUME_SSTATE)\n"
	@docker volume create $(VOLUME_DOWNLOADS) >/dev/null && printf "  Created $(VOLUME_DOWNLOADS)\n"
	@docker volume create $(VOLUME_CACHE) >/dev/null && printf "  Created $(VOLUME_CACHE)\n"
	@printf "$(GREEN)Volumes created.$(NC)\n"

volumes-remove:
	@if [ "$$(uname)" != "Darwin" ]; then \
		printf "$(YELLOW)Volumes only used on macOS. Skipping.$(NC)\n"; \
		exit 0; \
	fi
	@printf "$(RED)WARNING: This will delete all build data in Docker volumes.$(NC)\n"
	@read -p "Are you sure? [y/N] " confirm && \
		if [ "$$confirm" = "y" ] || [ "$$confirm" = "Y" ]; then \
			docker volume rm $(VOLUME_BUILD) $(VOLUME_SSTATE) $(VOLUME_DOWNLOADS) $(VOLUME_CACHE) 2>/dev/null || true; \
			printf "$(GREEN)Volumes removed.$(NC)\n"; \
		else \
			printf "Cancelled.\n"; \
		fi

#
# Deploy tegraflash tarball (macOS)
#
deploy: _check-machine
	@if [ "$$(uname)" != "Darwin" ]; then \
		printf "Images are already on host filesystem at:\n"; \
		printf "  $(PROJECT_DIR)/build/tmp/deploy/images/$(MACHINE)/\n"; \
		exit 0; \
	fi
	@printf "$(CYAN)Copying tegraflash tarball from Docker volume...$(NC)\n"
	@mkdir -p $(PROJECT_DIR)/deploy
	@rm -f $(PROJECT_DIR)/deploy/wendyos.img
	@docker run --rm -t \
		-v $(VOLUME_BUILD):/build-volume:ro \
		-v $(PROJECT_DIR)/deploy:/output \
		$(DOCKER_REPO):$(DOCKER_TAG) \
		/bin/bash -c '\
			TARBALL="/build-volume/deploy/images/$(MACHINE)/$(IMAGE_TARGET)-$(MACHINE).tegraflash.tar.gz"; \
			if [ -f "$$TARBALL" ]; then \
				rsync -ahL --progress "$$TARBALL" /output/; \
			else \
				echo "Error: tegraflash tarball not found. Run make build first."; \
				exit 1; \
			fi \
		'
	@printf "$(GREEN)Tegraflash tarball copied to: $(PROJECT_DIR)/deploy/$(NC)\n"

#
# Flash Commands
#
flash-to-external: _check-machine
	@printf "$(CYAN)WendyOS Flash Tool$(NC)\n"
	@printf "$(CYAN)==================$(NC)\n\n"
	@OS_TYPE=$$(uname); \
	if [ "$$OS_TYPE" = "Darwin" ]; then DD_BS="4m"; else DD_BS="4M"; fi; \
	if [ -f "$(PROJECT_DIR)/deploy/wendyos.img" ]; then \
		IMG_SIZE=$$(ls -lh "$(PROJECT_DIR)/deploy/wendyos.img" | awk '{print $$5}'); \
		printf "Using existing image: $(PROJECT_DIR)/deploy/wendyos.img ($$IMG_SIZE)\n\n"; \
	elif echo "$(MACHINE)" | grep -q "raspberrypi"; then \
		WIC_IMG="$(PROJECT_DIR)/build/tmp/deploy/images/$(MACHINE)/$(IMAGE_TARGET)-$(MACHINE).rootfs.wic"; \
		if [ ! -f "$$WIC_IMG" ]; then \
			printf "$(RED)Error: WIC image not found: $$WIC_IMG$(NC)\n"; \
			printf "Run 'make build MACHINE=$(MACHINE)' first.\n"; \
			exit 1; \
		fi; \
		mkdir -p "$(PROJECT_DIR)/deploy"; \
		cp "$$WIC_IMG" "$(PROJECT_DIR)/deploy/wendyos.img"; \
		printf "$(GREEN)RPi5 WIC image ready: $(PROJECT_DIR)/deploy/wendyos.img$(NC)\n\n"; \
	else \
		if [ "$$OS_TYPE" = "Darwin" ]; then \
			if [ ! -f "$(PROJECT_DIR)/deploy/$(IMAGE_TARGET)-$(MACHINE).tegraflash.tar.gz" ]; then \
				printf "Fetching tegraflash tarball from Docker volume...\n"; \
				$(MAKE) deploy; \
			fi; \
		fi; \
		TEGRAFLASH="$(PROJECT_DIR)/deploy/$(IMAGE_TARGET)-$(MACHINE).tegraflash.tar.gz"; \
		if [ ! -f "$$TEGRAFLASH" ]; then \
			if [ "$$OS_TYPE" != "Darwin" ] && [ -f "$(PROJECT_DIR)/build/tmp/deploy/images/$(MACHINE)/$(IMAGE_TARGET)-$(MACHINE).tegraflash.tar.gz" ]; then \
				TEGRAFLASH="$(PROJECT_DIR)/build/tmp/deploy/images/$(MACHINE)/$(IMAGE_TARGET)-$(MACHINE).tegraflash.tar.gz"; \
				mkdir -p "$(PROJECT_DIR)/deploy"; \
			else \
				printf "$(RED)Error: tegraflash package not found.$(NC)\n"; \
				printf "Run 'make build' first.\n"; \
				exit 1; \
			fi; \
		fi; \
		printf "$(CYAN)Creating flashable image...$(NC)\n"; \
		mkdir -p $(PROJECT_DIR)/deploy/flash-work; \
		printf "Extracting tegraflash package...\n"; \
		tar -xzf "$$TEGRAFLASH" -C $(PROJECT_DIR)/deploy/flash-work; \
		printf "Creating $(FLASH_IMAGE_SIZE) image file (this may take a while)...\n"; \
		if [ "$$OS_TYPE" = "Darwin" ]; then \
			docker run --rm -t \
				--privileged \
				-v $(PROJECT_DIR)/deploy/flash-work:/flash \
				-v $(PROJECT_DIR)/deploy:/output \
				$(DOCKER_REPO):$(DOCKER_TAG) \
				/bin/bash -c '\
					cd /flash && \
					sudo ./doexternal.sh -s $(FLASH_IMAGE_SIZE) /output/wendyos.img \
				' || { rm -rf $(PROJECT_DIR)/deploy/flash-work; printf "\n$(RED)Image creation FAILED. Check doexternal.sh output above.$(NC)\n"; exit 1; }; \
		else \
			cd $(PROJECT_DIR)/deploy/flash-work && \
			sudo ./doexternal.sh -s $(FLASH_IMAGE_SIZE) $(PROJECT_DIR)/deploy/wendyos.img || { rm -rf $(PROJECT_DIR)/deploy/flash-work; printf "\n$(RED)Image creation FAILED. Check doexternal.sh output above.$(NC)\n"; exit 1; }; \
		fi; \
		rm -rf $(PROJECT_DIR)/deploy/flash-work; \
		printf "\n$(GREEN)Image created: $(PROJECT_DIR)/deploy/wendyos.img$(NC)\n\n"; \
	fi; \
	if [ -n "$(FLASH_DEVICE)" ] && [ "$(FLASH_CONFIRM)" = "yes" ]; then \
		printf "$(YELLOW)Non-interactive mode: flashing to $(FLASH_DEVICE)$(NC)\n\n"; \
	else \
		printf "$(YELLOW)Available external disks:$(NC)\n\n"; \
		if [ "$$OS_TYPE" = "Darwin" ]; then \
			diskutil list external physical 2>/dev/null || printf "No external disks found.\n"; \
		else \
			lsblk -d -o NAME,SIZE,MODEL,TRAN 2>/dev/null | grep -E "usb|sata|nvme" || \
			lsblk -d -o NAME,SIZE,MODEL 2>/dev/null | grep -vE "^(loop|sr|ram)" | head -20; \
		fi; \
		printf "\n"; \
	fi; \
	if [ -n "$(FLASH_DEVICE)" ]; then \
		DEVICE="$(FLASH_DEVICE)"; \
	else \
		if [ "$$OS_TYPE" = "Darwin" ]; then \
			printf "$(YELLOW)Enter the disk to flash (e.g., disk42) or 'q' to quit:$(NC) "; \
		else \
			printf "$(YELLOW)Enter the disk to flash (e.g., sdb, nvme0n1) or 'q' to quit:$(NC) "; \
		fi; \
		read device_input; \
		if [ "$$device_input" = "q" ] || [ "$$device_input" = "Q" ]; then \
			printf "\nCancelled. Image saved at: $(PROJECT_DIR)/deploy/wendyos.img\n"; \
			exit 0; \
		fi; \
		if [ "$$OS_TYPE" = "Darwin" ]; then \
			case "$$device_input" in \
				disk[0-9]*) DEVICE="/dev/$$device_input" ;; \
				/dev/disk[0-9]*) DEVICE="$$device_input" ;; \
				*) printf "$(RED)Error: Invalid disk name '$$device_input'. Must be like 'disk42' or '/dev/disk42'$(NC)\n"; exit 1 ;; \
			esac; \
		else \
			case "$$device_input" in \
				sd[a-z]|sd[a-z][a-z]|nvme[0-9]n[0-9]|nvme[0-9][0-9]n[0-9]) DEVICE="/dev/$$device_input" ;; \
				/dev/sd[a-z]*|/dev/nvme*) DEVICE="$$device_input" ;; \
				*) printf "$(RED)Error: Invalid disk name '$$device_input'. Must be like 'sdb', 'nvme0n1', or '/dev/sdb'$(NC)\n"; exit 1 ;; \
			esac; \
		fi; \
	fi; \
	if [ ! -e "$$DEVICE" ]; then \
		printf "$(RED)Error: Device $$DEVICE does not exist.$(NC)\n"; \
		exit 1; \
	fi; \
	printf "\n"; \
	printf "$(RED)WARNING: This will ERASE ALL DATA on $$DEVICE!$(NC)\n"; \
	if [ "$$OS_TYPE" = "Darwin" ]; then \
		diskutil info "$$DEVICE" 2>/dev/null | grep -E "Device / Media Name|Disk Size" || true; \
	else \
		lsblk -o NAME,SIZE,MODEL "$$DEVICE" 2>/dev/null || true; \
	fi; \
	printf "\n"; \
	if [ "$(FLASH_CONFIRM)" = "yes" ]; then \
		confirm="yes"; \
	else \
		printf "$(YELLOW)Type 'yes' to confirm:$(NC) "; \
		read confirm; \
	fi; \
	if [ "$$confirm" = "yes" ]; then \
		printf "\n$(CYAN)Unmounting $$DEVICE...$(NC)\n"; \
		if [ "$$OS_TYPE" = "Darwin" ]; then \
			diskutil unmountDisk "$$DEVICE" 2>/dev/null || true; \
			RAW_DEVICE=$$(echo "$$DEVICE" | sed 's|/dev/disk|/dev/rdisk|'); \
		else \
			sudo umount "$$DEVICE"* 2>/dev/null || true; \
			RAW_DEVICE="$$DEVICE"; \
		fi; \
		printf "$(CYAN)Flashing image to $$RAW_DEVICE...$(NC)\n"; \
		printf "This may take 5-15 minutes depending on drive speed.\n\n"; \
		if sudo dd if=$(PROJECT_DIR)/deploy/wendyos.img of="$$RAW_DEVICE" bs=$$DD_BS status=progress; then \
			sync; \
			printf "\n$(GREEN)Flash complete!$(NC)\n"; \
			printf "You can now safely eject the drive and insert it into your Jetson.\n"; \
			if [ "$$OS_TYPE" = "Darwin" ]; then \
				diskutil eject "$$DEVICE" 2>/dev/null || true; \
			else \
				sudo eject "$$DEVICE" 2>/dev/null || udisksctl power-off -b "$$DEVICE" 2>/dev/null || true; \
			fi; \
		else \
			printf "\n$(RED)Flash FAILED! Check the error above.$(NC)\n"; \
			exit 1; \
		fi; \
	else \
		printf "\nCancelled. Image saved at: $(PROJECT_DIR)/deploy/wendyos.img\n"; \
	fi

#
# Internal Targets
#
_check-machine:
	@if [ -z "$(MACHINE)" ]; then \
		printf "$(RED)Error: MACHINE is required.$(NC)\n\n"; \
		printf "Usage:\n"; \
		printf "  make $(MAKECMDGOALS) MACHINE=<machine>\n\n"; \
		printf "Available machines:\n"; \
		for m in $(MAKEFILE_DIR)/conf/machine/*.conf; do \
			printf "  %s\n" "$$(basename $$m .conf)"; \
		done; \
		printf "\n"; \
		exit 1; \
	fi

_check-setup:
	@if [ ! -d "$(DOCKER_DIR)" ]; then \
		printf "$(RED)Error: Build environment not set up.$(NC)\n"; \
		printf "Run 'make setup' first.\n"; \
		exit 1; \
	fi
	@if ! docker image inspect $(DOCKER_REPO):$(DOCKER_TAG) >/dev/null 2>&1; then \
		printf "$(RED)Error: Docker image not found.$(NC)\n"; \
		printf "Run 'make setup' or 'make docker-create' first.\n"; \
		exit 1; \
	fi

_ensure-volumes:
	@if [ "$$(uname)" = "Darwin" ]; then \
		docker volume inspect $(VOLUME_BUILD) >/dev/null 2>&1 || docker volume create $(VOLUME_BUILD) >/dev/null; \
		docker volume inspect $(VOLUME_SSTATE) >/dev/null 2>&1 || docker volume create $(VOLUME_SSTATE) >/dev/null; \
		docker volume inspect $(VOLUME_DOWNLOADS) >/dev/null 2>&1 || docker volume create $(VOLUME_DOWNLOADS) >/dev/null; \
		docker volume inspect $(VOLUME_CACHE) >/dev/null 2>&1 || docker volume create $(VOLUME_CACHE) >/dev/null; \
		docker run --rm \
			-v $(VOLUME_BUILD):/vol/build \
			-v $(VOLUME_SSTATE):/vol/sstate \
			-v $(VOLUME_DOWNLOADS):/vol/downloads \
			-v $(VOLUME_CACHE):/vol/cache \
			$(DOCKER_REPO):$(DOCKER_TAG) \
			/bin/bash -c 'sudo chown -R $$(id -u):$$(id -g) /vol/build /vol/sstate /vol/downloads /vol/cache' 2>/dev/null || true; \
	fi

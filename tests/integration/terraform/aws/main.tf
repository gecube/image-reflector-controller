provider "aws" {}

resource "random_pet" "suffix" {}

locals {
  name = "flux-test-${random_pet.suffix.id}"
}

module "eks" {
  source = "git::https://gitlab.com/darkowlzz/flux-test-infra.git//modules/aws/eks"

  name = local.name
}

module "ecr" {
  source = "git::https://gitlab.com/darkowlzz/flux-test-infra.git//modules/aws/ecr"

  name = local.name
}

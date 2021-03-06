# Example NA12878 TruSeq variant caller 
This example pipeline uses GATK to call variants in the popular NA12878
dataset. The pipeline is based on 
[Cornish, A. and Guda, C., 2015. A comparison of variant calling pipelines using genome in a bottle as a reference. BioMed research international, 2015.](https://www.ncbi.nlm.nih.gov/pmc/articles/PMC4619817/).


# Run 

```
walrus -i pipeline.json
```

# Data 
Since [Github supports files up to 2GiB](https://github.com/git-lfs/git-lfs/wiki/Implementations) 
we have not added the datasets to the walrus repository. The datasets are
available online and you can download the reference from
ftp://ftp-trace.ncbi.nih.gov/1000genomes/ftp/technical/reference the NA12878
data from https://www.ebi.ac.uk/ena/data/view/SRR098401.

Put all files in a `data/` folder where walrus can find them. 

